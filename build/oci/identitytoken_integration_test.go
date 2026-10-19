package oci_test

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	"github.com/stretchr/testify/require"
)

// Run in the standard builder image with FAST_SANDBOX_TEST_NATIVE_OCI=1.
// No KVM, mount privileges or ublk kernel support are required: overlaybd-apply
// opens a real remote native lower filesystem and writes a private upper.
func TestNativeOverlayBDIdentityTokenReads(t *testing.T) {
	if os.Getenv("FAST_SANDBOX_TEST_NATIVE_OCI") != "1" {
		t.Skip("requires the built OCI native tools")
	}
	dir := t.TempDir()
	run := func(tool string, args ...string) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		output, err := exec.CommandContext(ctx, tool, args...).CombinedOutput()
		if err != nil {
			logs, _ := filepath.Glob(filepath.Join(dir, "*.log"))
			for _, file := range logs {
				raw, _ := os.ReadFile(file)
				if len(raw) > 16384 {
					raw = raw[len(raw)-16384:]
				}
				t.Logf("native log %s: %s", filepath.Base(file), raw)
			}
		}
		require.NoError(t, err, "%s: %s", tool, output)
	}
	data, index, lower := filepath.Join(dir, "base.data"), filepath.Join(dir, "base.index"), filepath.Join(dir, "base.commit")
	run("overlaybd-create", "-s", "--mkfs", data, index, "1")
	run("overlaybd-commit", "-z", data, index, lower)
	payload, err := os.ReadFile(lower)
	require.NoError(t, err)
	sum := digest.FromBytes(payload).String()
	var exchanges, reads atomic.Int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			if r.Method != http.MethodPost || r.ParseForm() != nil || r.Form.Get("refresh_token") != "refresh+a&b=secret" || r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("scope") != "repository:team/image:pull" || r.Form.Get("service") != "native-test" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			exchanges.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"native-access","expires_in":60}`))
			return
		}
		if r.Header.Get("Authorization") != "Bearer native-access" {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="native-test",scope="repository:team/image:pull"`, server.URL))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/v2/team/image/blobs/"+sum {
			http.NotFound(w, r)
			return
		}
		reads.Add(1)
		http.ServeContent(w, r, "base.commit", time.Time{}, bytes.NewReader(payload))
	}))
	defer server.Close()
	writeJSON := func(name string, value any) string {
		raw, err := json.Marshal(value)
		require.NoError(t, err)
		file := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(file, raw, 0600))
		return file
	}
	cred := writeJSON("cred.json", map[string]any{"auths": map[string]any{strings.TrimPrefix(server.URL, "http://") + "/team/image": map[string]string{"identitytoken": "refresh+a&b=secret"}}})
	emptyTar := filepath.Join(dir, "empty.tar")
	file, err := os.Create(emptyTar)
	require.NoError(t, err)
	require.NoError(t, tar.NewWriter(file).Close())
	require.NoError(t, file.Close())
	for round := range 2 {
		label := strconv.Itoa(round)
		upperData, upperIndex := filepath.Join(dir, label+".data"), filepath.Join(dir, label+".index")
		run("overlaybd-create", "-s", upperData, upperIndex, "1")
		config := writeJSON(label+".json", map[string]any{"repoBlobUrl": server.URL + "/v2/team/image/blobs", "lowers": []map[string]any{{"digest": sum, "size": len(payload)}}, "upper": map[string]any{"data": upperData, "index": upperIndex}})
		service := writeJSON(label+"-service.json", map[string]any{"registryFsVersion": "v2", "credentialConfig": map[string]any{"mode": "file", "path": cred}, "cacheConfig": map[string]any{"cacheType": "file", "cacheDir": filepath.Join(dir, label+"-cache"), "cacheSizeGB": 1}, "download": map[string]bool{"enable": false}, "logConfig": map[string]any{"logLevel": 1, "logPath": filepath.Join(dir, label+".log")}})
		before := reads.Load()
		run("overlaybd-apply", "--service_config_path", service, emptyTar, config)
		require.Greater(t, reads.Load(), before, "each new reader must fetch actual lower blocks")
	}
	require.GreaterOrEqual(t, exchanges.Load(), int32(2), "retain the refresh credential for subsequent readers")
}

func TestNativeRawImporterRoundTrip(t *testing.T) {
	if os.Getenv("FAST_SANDBOX_TEST_NATIVE_OCI") != "1" {
		t.Skip("requires the built OCI native tools")
	}
	dir := t.TempDir()
	source := filepath.Join(dir, "source.raw")
	file, err := os.Create(source)
	require.NoError(t, err)
	require.NoError(t, file.Truncate(1<<20))
	_, err = file.WriteAt(bytes.Repeat([]byte("checkpoint-data"), 256), 4096)
	require.NoError(t, err)
	require.NoError(t, file.Close())
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	commit := filepath.Join(dir, "sealed.commit")
	output, err := exec.CommandContext(ctx, "overlaybd-import-raw", source, filepath.Join(dir, "data"), filepath.Join(dir, "index"), commit).CombinedOutput()
	require.NoError(t, err, "%s", output)
	// The importer reopens the sealed layer and verifies every byte against
	// the source before emitting this marker, including skipped sparse holes.
	require.Contains(t, string(output), "OVERLAYBD_IMPORT_SUCCEEDED logical_bytes=1048576")
	require.FileExists(t, commit)
}
