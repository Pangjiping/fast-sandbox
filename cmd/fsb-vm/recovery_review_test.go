package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestInjectionPreservesGuestHostsAndConfinesSymlinks(t *testing.T) {
	for _, mode := range []string{"existing", "missing", "symlink", "escaping-parent"} {
		t.Run(mode, func(t *testing.T) {
			guest, source, outside := t.TempDir(), t.TempDir(), t.TempDir()
			require.NoError(t, os.MkdirAll(filepath.Join(source, "etc"), 0755))
			require.NoError(t, os.WriteFile(filepath.Join(source, "etc/hosts"), []byte("default"), 0644))
			sentinel := filepath.Join(outside, "hosts")
			require.NoError(t, os.WriteFile(sentinel, []byte("host-private"), 0600))
			if mode == "escaping-parent" {
				require.NoError(t, os.Symlink(outside, filepath.Join(guest, "etc")))
			} else {
				require.NoError(t, os.MkdirAll(filepath.Join(guest, "etc"), 0755))
				if mode == "existing" {
					require.NoError(t, os.WriteFile(filepath.Join(guest, "etc/hosts"), []byte("10.0.0.1 custom"), 0644))
				}
				if mode == "symlink" {
					require.NoError(t, os.Symlink(sentinel, filepath.Join(guest, "etc/hosts")))
				}
			}
			err := copyInjection(guest, source)
			if mode == "escaping-parent" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				if mode == "symlink" {
					target, e := os.Readlink(filepath.Join(guest, "etc/hosts"))
					require.NoError(t, e)
					require.Equal(t, sentinel, target)
				} else {
					data, e := os.ReadFile(filepath.Join(guest, "etc/hosts"))
					require.NoError(t, e)
					want := "default"
					if mode == "existing" {
						want = "10.0.0.1 custom"
					}
					require.Equal(t, want, string(data))
				}
			}
			raw, err := os.ReadFile(sentinel)
			require.NoError(t, err)
			require.Equal(t, "host-private", string(raw))
		})
	}
}
func TestReleasedPackMissingWorkIsIdempotentButOtherPhasesAreRetained(t *testing.T) {
	for _, mode := range []string{"released", "adding", "attached", "prepared", "outside", "dangling-link"} {
		t.Run(mode, func(t *testing.T) {
			previous := global
			t.Cleanup(func() { global = previous })
			bundle := t.TempDir()
			global.buildWorkDir = t.TempDir()
			work := filepath.Join(global.buildWorkDir, "snapshot_missing")
			phase := mode
			if mode == "outside" || mode == "dangling-link" {
				phase = packPhaseReleased
			}
			if mode == "outside" {
				work = filepath.Join(t.TempDir(), "snapshot_missing")
			}
			if mode == "dangling-link" {
				require.NoError(t, os.Symlink(filepath.Join(t.TempDir(), "missing"), work))
			}
			marker := filepath.Join(bundle, packActiveMarker)
			require.NoError(t, writePackResources(marker, work, phase))
			err := recoverPack(context.Background(), bundle, &fakeReleaser{}, func(string) error { t.Fatal("must not unmount absent work"); return nil })
			if mode == "released" {
				require.NoError(t, err)
				require.NoFileExists(t, marker)
				require.NoError(t, recoverPack(context.Background(), bundle, &fakeReleaser{}, nil))
			} else {
				require.Error(t, err)
				require.FileExists(t, marker)
			}
		})
	}
}

type fenceTransport func(*http.Request) (*http.Response, error)

func (f fenceTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func fenceReply(code int, body string) *http.Response {
	return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}
func TestPackRecoveryFencesLateAddBeforeReleasingFiles(t *testing.T) {
	for _, mode := range []string{"never-sent", "legacy", "late-add", "disconnect", "unknown-reply", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			previous := global
			t.Cleanup(func() { global = previous })
			global.buildWorkDir = t.TempDir()
			bundle := t.TempDir()
			work := filepath.Join(global.buildWorkDir, "snapshot_test")
			require.NoError(t, os.MkdirAll(work, 0750))
			config := filepath.Join(work, snapfilesConfigName)
			require.NoError(t, os.WriteFile(config, []byte(`{"valid":"original"}`), 0600))
			backing := filepath.Join(work, "backing")
			require.NoError(t, os.WriteFile(backing, []byte("must retain until Del"), 0600))
			marker := filepath.Join(bundle, packActiveMarker)
			require.NoError(t, writePackResources(marker, work, packPhaseAdding))
			if mode == "legacy" {
				require.NoError(t, os.WriteFile(marker, []byte(work), 0600))
			}
			calls, deleted := 0, false
			c := &ublkdClient{httpClient: &http.Client{Transport: fenceTransport(func(req *http.Request) (*http.Response, error) {
				require.FileExists(t, backing)
				switch req.URL.Path {
				case "/v1/add":
					var input map[string]string
					require.NoError(t, json.NewDecoder(req.Body).Decode(&input))
					require.Equal(t, config, input["config"])
					raw, err := os.ReadFile(config)
					require.NoError(t, err)
					require.False(t, json.Valid(raw), "future Adds must be disabled first")
					calls++
					if mode == "disconnect" {
						return nil, errors.New("lost response")
					}
					if mode == "unknown-reply" {
						return fenceReply(500, `{"error":"unrecognized daemon"}`), nil
					}
					if mode == "timeout" {
						<-req.Context().Done()
						return nil, req.Context().Err()
					}
					if mode == "late-add" && calls == 1 {
						return fenceReply(409, `{"error":"an add of this image is already in progress"}`), nil
					}
					return fenceReply(500, `{"error":"device failed to start (see daemon log)"}`), nil
				case "/v1/list":
					require.Greater(t, calls, 0, "List alone cannot fence an Add")
					if mode == "late-add" && !deleted {
						return fenceReply(200, `{"ok":true,"devices":[{"dev_id":42,"config":`+strconv.Quote(config)+`}]}`), nil
					}
					return fenceReply(200, `{"ok":true,"devices":[]}`), nil
				case "/v1/del":
					var input map[string]int
					require.NoError(t, json.NewDecoder(req.Body).Decode(&input))
					require.Equal(t, 42, input["dev_id"])
					deleted = true
					return fenceReply(200, `{"ok":true}`), nil
				default:
					t.Fatal(req.URL.Path)
					return nil, nil
				}
			})}}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if mode == "timeout" {
				ctx, cancel = context.WithTimeout(context.Background(), 10*time.Millisecond)
				defer cancel()
			}
			err := recoverPack(ctx, bundle, c, func(string) error { return nil })
			if mode == "never-sent" || mode == "legacy" || mode == "late-add" {
				require.NoError(t, err)
				require.NoDirExists(t, work)
				require.NoFileExists(t, marker)
				if mode == "late-add" {
					require.True(t, deleted)
					require.Equal(t, 2, calls)
				}
			} else {
				require.Error(t, err)
				require.FileExists(t, backing)
				require.FileExists(t, marker)
			}
		})
	}
}
