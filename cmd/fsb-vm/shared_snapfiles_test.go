package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type snapfilesTransport struct{ url *url.URL }

func (t snapfilesTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r.URL.Scheme, r.URL.Host = t.url.Scheme, t.url.Host
	return http.DefaultTransport.RoundTrip(r)
}

func sharedSnapfilesFixture(t *testing.T) (*ublkdClient, *int, *int) {
	t.Helper()
	setupManifestCache(t)
	global.runtimeDir = t.TempDir()
	bin := t.TempDir()
	// A procfs symlink gives isMounted a distinct filesystem without CAP_SYS_ADMIN.
	require.NoError(t, os.WriteFile(filepath.Join(bin, "mount"), []byte("#!/bin/sh\nset -eu\nrmdir \"$6\"\nln -s /proc \"$6\"\n"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(bin, "umount"), []byte("#!/bin/sh\nset -eu\nrm \"$1\"\n"), 0755))
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	var devices []ublkdDevice
	adds, deletes := new(int), new(int)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/add":
			var body struct {
				Config string `json:"config"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			*adds++
			d := ublkdDevice{DevID: *adds, Dev: "/dev/ublkb" + strings.Repeat("1", *adds), Config: body.Config}
			devices = append(devices, d)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "dev_id": d.DevID, "dev": d.Dev})
		case "/v1/list":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "devices": devices})
		case "/v1/del":
			var body struct {
				ID int `json:"dev_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			for i, d := range devices {
				if d.DevID == body.ID {
					devices = append(devices[:i], devices[i+1:]...)
					break
				}
			}
			*deletes++
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		default:
			http.Error(w, "unexpected request", 500)
		}
	}))
	t.Cleanup(server.Close)
	target, err := url.Parse(server.URL)
	require.NoError(t, err)
	return &ublkdClient{httpClient: &http.Client{Transport: snapfilesTransport{target}}}, adds, deletes
}

func TestSharedSnapfilesConcurrentAcquireAndLeaseCleanup(t *testing.T) {
	client, adds, deletes := sharedSnapfilesFixture(t)
	image := resolvedImage{RepoBlobURL: "https://r.example/v2/team/t/blobs", Lowers: []lowerLayer{{Digest: "sha256:" + strings.Repeat("a", 64), Size: 4096}}}
	states := []*sandboxState{{SandboxID: "first", SnapfilesRef: "r.example/team/t:v1"}, {SandboxID: "second", SnapfilesRef: "r.example/team/t:v1"}}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, state := range states {
		require.NoError(t, os.MkdirAll(global.sandboxHome(state.SandboxID), 0750))
		require.NoError(t, writeState(global.sandboxHome(state.SandboxID), state))
		wg.Add(1)
		go func(s *sandboxState) {
			defer wg.Done()
			results <- acquireSharedSnapfiles(context.Background(), client, global.sandboxHome(s.SandboxID), s, image, dockerConfig{})
		}(state)
	}
	wg.Wait()
	close(results)
	for err := range results {
		require.NoError(t, err)
	}
	require.Equal(t, 1, *adds)
	require.Equal(t, states[0].MountPoint, states[1].MountPoint)
	require.Nil(t, states[0].Snapfiles)
	require.ErrorContains(t, pruneSharedSnapfiles(context.Background(), client, true), "leases")
	states[0].SharedSnapfiles = ""
	require.NoError(t, writeState(global.sandboxHome("first"), states[0]))
	require.ErrorContains(t, pruneSharedSnapfiles(context.Background(), client, true), "leases")
	require.Zero(t, *deletes)
	states[1].SharedSnapfiles = ""
	require.NoError(t, writeState(global.sandboxHome("second"), states[1]))
	require.NoError(t, pruneSharedSnapfiles(context.Background(), client, false))
	require.Zero(t, *deletes, "idle device must stay warm")
	require.NoError(t, pruneSharedSnapfiles(context.Background(), client, true))
	require.Equal(t, 1, *deletes)
	require.NoError(t, pruneSharedSnapfiles(context.Background(), client, true))
	require.Equal(t, 1, *deletes)
}

func TestSharedSnapfilesScopeAndChangedLayers(t *testing.T) {
	setupManifestCache(t)
	image := resolvedImage{Lowers: []lowerLayer{{Digest: "one", Size: 1}}}
	auth := dockerConfig{Auths: map[string]authEntry{"r.example": {Username: "reader", Password: "first"}}}
	first, err := sharedSnapfilesKey("r.example/team/t:v1", image, auth)
	require.NoError(t, err)
	same, err := sharedSnapfilesKey("r.example/team/t:v2", image, auth)
	require.NoError(t, err)
	require.Equal(t, first, same)
	auth.Auths["r.example"] = authEntry{Username: "reader", Password: "rotated"}
	changed, err := sharedSnapfilesKey("r.example/team/t:v1", image, auth)
	require.NoError(t, err)
	require.NotEqual(t, first, changed)
	image.Lowers[0].Digest = "different"
	layers, err := sharedSnapfilesKey("r.example/team/t:v1", image, auth)
	require.NoError(t, err)
	require.NotEqual(t, changed, layers)
}

func TestSharedSnapfilesLockCancellation(t *testing.T) {
	setupManifestCache(t)
	unlock, err := lockSharedSnapfiles(context.Background(), "key")
	require.NoError(t, err)
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	_, err = lockSharedSnapfiles(ctx, "key")
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestSharedSnapfilesDeleteKeepsOtherLeaseAndRetainsFailedRelease(t *testing.T) {
	layout, state := newReleaseFixture(t)
	state.SharedSnapfiles = strings.Repeat("a", 64)
	state.Snapfiles = nil
	releaser := &fakeReleaser{devices: []ublkdDevice{{DevID: 4, Config: layout.rootfsConfig}, {DevID: 5, Config: "/shared/snapfiles.config"}}, delErr: map[int]error{4: errors.New("busy")}}
	require.ErrorContains(t, releaseSandbox(context.Background(), releaser, layout, state), "busy")
	require.NotEmpty(t, state.SharedSnapfiles)
	releaser.delErr = nil
	require.NoError(t, releaseSandbox(context.Background(), releaser, layout, state))
	require.Empty(t, state.SharedSnapfiles)
	require.Equal(t, []int{4}, releaser.deleted)
}

func TestSharedSnapfilesMissingDeviceFailsClosed(t *testing.T) {
	client, adds, _ := sharedSnapfilesFixture(t)
	image := resolvedImage{RepoBlobURL: "https://r.example/v2/team/t/blobs"}
	state := &sandboxState{SandboxID: "first", SnapfilesRef: "r.example/team/t:v1"}
	require.NoError(t, os.MkdirAll(global.sandboxHome("first"), 0750))
	require.NoError(t, acquireSharedSnapfiles(context.Background(), client, global.sandboxHome("first"), state, image, dockerConfig{}))
	require.NoError(t, client.Del(context.Background(), 1))
	second := &sandboxState{SandboxID: "second", SnapfilesRef: state.SnapfilesRef}
	require.NoError(t, os.MkdirAll(global.sandboxHome("second"), 0750))
	require.ErrorContains(t, acquireSharedSnapfiles(context.Background(), client, global.sandboxHome("second"), second, image, dockerConfig{}), "device missing")
	require.Equal(t, 1, *adds, "must not silently replace a device held by another VM")
}

func TestSharedSnapfilesPrunesExpiredOnlyWithoutLeases(t *testing.T) {
	client, _, deletes := sharedSnapfilesFixture(t)
	state := &sandboxState{SandboxID: "first", SnapfilesRef: "r.example/team/t:v1"}
	require.NoError(t, os.MkdirAll(global.sandboxHome("first"), 0750))
	require.NoError(t, acquireSharedSnapfiles(context.Background(), client, global.sandboxHome("first"), state, resolvedImage{}, dockerConfig{}))
	path := statePath(filepath.Dir(state.MountPoint))
	old := time.Now().Add(-2 * sharedSnapfilesIdleTTL)
	require.NoError(t, os.Chtimes(path, old, old))
	require.NoError(t, pruneSharedSnapfiles(context.Background(), client, false))
	require.Zero(t, *deletes)
	state.SharedSnapfiles = ""
	require.NoError(t, writeState(global.sandboxHome("first"), state))
	require.NoError(t, pruneSharedSnapfiles(context.Background(), client, false))
	require.Equal(t, 1, *deletes)
}

func TestSharedSnapfilesDeleteRetryDoesNotUnmountCache(t *testing.T) {
	client, _, deletes := sharedSnapfilesFixture(t)
	state := &sandboxState{SandboxID: "first", SnapfilesRef: "r.example/team/t:v1"}
	layout := newSandboxLayout("first")
	require.NoError(t, claimSandbox(layout, "first"))
	require.NoError(t, acquireSharedSnapfiles(context.Background(), client, layout.home, state, resolvedImage{}, dockerConfig{}))
	mountPoint := state.MountPoint
	require.NoError(t, releaseSandbox(context.Background(), client, layout, state))
	require.Empty(t, state.SharedSnapfiles)
	require.NoFileExists(t, layout.mountPoint)
	require.NoError(t, releaseSandbox(context.Background(), client, layout, state))
	require.True(t, isMounted(mountPoint))
	require.Zero(t, *deletes)
}

func TestSharedSnapfilesBoundsIdleCache(t *testing.T) {
	client, _, deletes := sharedSnapfilesFixture(t)
	for i := 0; i < sharedSnapfilesMaxIdle+2; i++ {
		id := fmt.Sprintf("sandbox-%d", i)
		state := &sandboxState{SandboxID: id, SnapfilesRef: "r.example/team/t:v1"}
		require.NoError(t, os.MkdirAll(global.sandboxHome(id), 0750))
		require.NoError(t, acquireSharedSnapfiles(context.Background(), client, global.sandboxHome(id), state, resolvedImage{Lowers: []lowerLayer{{Digest: id, Size: 1}}}, dockerConfig{}))
		state.SharedSnapfiles = ""
		require.NoError(t, writeState(global.sandboxHome(id), state))
	}
	require.NoError(t, pruneSharedSnapfiles(context.Background(), client, false))
	require.Equal(t, 2, *deletes)
}

func TestSharedSnapfilesAliasProtectsLostState(t *testing.T) {
	client, _, deletes := sharedSnapfilesFixture(t)
	layout := newSandboxLayout("first")
	require.NoError(t, claimSandbox(layout, "first"))
	state := &sandboxState{SandboxID: "first", SnapfilesRef: "r.example/team/t:v1"}
	require.NoError(t, acquireSharedSnapfiles(context.Background(), client, layout.home, state, resolvedImage{}, dockerConfig{}))
	key, mount := state.SharedSnapfiles, state.MountPoint
	require.NoError(t, os.Remove(statePath(layout.home)))
	referenced, err := sharedSnapfilesReferenced(key)
	require.NoError(t, err)
	require.True(t, referenced)
	// delete's unreadable-state fallback must recover the shared ownership.
	recovered := &sandboxState{SandboxID: "first"}
	require.NoError(t, releaseSandbox(context.Background(), client, layout, recovered))
	require.True(t, isMounted(mount))
	require.Zero(t, *deletes)
}

func TestSharedSnapfilesRetentionStartsAtRelease(t *testing.T) {
	client, _, deletes := sharedSnapfilesFixture(t)
	layout := newSandboxLayout("first")
	require.NoError(t, claimSandbox(layout, "first"))
	state := &sandboxState{SandboxID: "first", SnapfilesRef: "r.example/team/t:v1"}
	require.NoError(t, acquireSharedSnapfiles(context.Background(), client, layout.home, state, resolvedImage{}, dockerConfig{}))
	path := statePath(filepath.Dir(state.MountPoint))
	old := time.Now().Add(-2 * sharedSnapfilesIdleTTL)
	require.NoError(t, os.Chtimes(path, old, old))
	require.NoError(t, releaseSandbox(context.Background(), client, layout, state))
	require.NoError(t, pruneSharedSnapfiles(context.Background(), client, false))
	require.Zero(t, *deletes, "a long-running VM must not make its newly idle template immediately expire")
}
