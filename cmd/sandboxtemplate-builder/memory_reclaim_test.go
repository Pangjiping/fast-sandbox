package main

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
)

func TestMemoryReclaimPolicy(t *testing.T) {
	if renderMemoryReclaim(nil) != "" {
		t.Fatal("disabled templates changed")
	}
	if validateMemoryReclaim(&apiv1alpha2.MemoryReclaimSpec{DamonReclaim: true}) == nil {
		t.Fatal("DAMON without reporting accepted")
	}
	p := &apiv1alpha2.MemoryReclaimSpec{FreePageReporting: true, DamonReclaim: true}
	if err := validateMemoryReclaim(p); err != nil {
		t.Fatal(err)
	}
	script := renderMemoryReclaim(p)
	for _, expected := range []string{"free_page_reporting_unsupported", "damon_reclaim_unsupported_", "echo Y > \"$damon/skip_anon\"", "echo 60000000", "echo 67108864", "echo 1000 > \"$damon/wmarks_high\"", "echo 1000 > \"$damon/wmarks_mid\"", "echo 0 > \"$damon/wmarks_low\""} {
		if !strings.Contains(script, expected) {
			t.Fatalf("missing safety/configuration %q", expected)
		}
	}
	p.DamonReclaim = false
	if strings.Contains(renderMemoryReclaim(p), "damon_reclaim/parameters") {
		t.Fatal("reporting-only template enabled DAMON")
	}
}

func TestFreePageReportingDeviceConfiguration(t *testing.T) {
	for _, test := range []struct {
		name      string
		enabled   bool
		rejectAPI bool
	}{
		{name: "disabled"}, {name: "enabled", enabled: true},
		{name: "unsupported VMM", enabled: true, rejectAPI: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir, err := os.MkdirTemp("", "fpr-")
			require.NoError(t, err)
			t.Cleanup(func() { _ = os.RemoveAll(dir) })
			socket := filepath.Join(dir, "api.sock")
			listener, err := net.Listen("unix", socket)
			require.NoError(t, err)
			var mu sync.Mutex
			var paths []string
			var balloon map[string]any
			server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				paths = append(paths, r.URL.Path)
				if r.URL.Path == "/balloon" {
					_ = json.NewDecoder(r.Body).Decode(&balloon)
					if test.rejectAPI {
						w.WriteHeader(http.StatusBadRequest)
						return
					}
				}
				w.WriteHeader(http.StatusNoContent)
			})}
			go func() { _ = server.Serve(listener) }()
			t.Cleanup(func() { _ = server.Close() })
			spec := apiv1alpha2.SandboxTemplateSpec{Machine: apiv1alpha2.MachineSpec{VCPU: "1", Memory: "1Gi"}}
			if test.enabled {
				spec.MemoryReclaim = &apiv1alpha2.MemoryReclaimSpec{FreePageReporting: true}
			}
			err = configureVM(&vmm{socket: socket}, "kernel", "rootfs", "args", spec, "")
			mu.Lock()
			defer mu.Unlock()
			if test.rejectAPI {
				require.ErrorContains(t, err, "configure free-page reporting")
				require.Equal(t, []string{"/machine-config", "/balloon"}, paths)
				return
			}
			require.NoError(t, err)
			if !test.enabled {
				require.NotContains(t, paths, "/balloon")
				return
			}
			require.Equal(t, "/balloon", paths[1], "device must be installed before boot")
			require.Equal(t, map[string]any{"amount_mib": float64(0), "deflate_on_oom": true,
				"stats_polling_interval_s": float64(1), "free_page_reporting": true}, balloon)
		})
	}
}
