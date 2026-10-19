package oci_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestEntrypointDaemonPolicyAndFailure(t *testing.T) {
	for _, test := range []struct {
		name, mode, spec string
		start, fail      bool
	}{
		{"runtime-default", "", "", true, false},
		{"disabled", "false", "", false, false},
		{"native-builder", "auto", `{"output":{"publish":"s3://bucket/template"}}`, false, false},
		{"oci-builder", "auto", `{"output":{"publish":"oci://registry/template:build"}}`, true, false},
		{"daemon-failure", "true", "", true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, script := range map[string]string{
				"mkdir": "#!/bin/sh\nexit 0\n",
				"curl":  "#!/bin/sh\ntest -e \"$TEST_OCI_DIR/ready\"\n",
				"overlaybd-ublkd": `#!/bin/sh
echo $$ > "$TEST_OCI_DIR/pid"
if [ "$TEST_OCI_FAIL" = true ]; then exit 7; fi
touch "$TEST_OCI_DIR/ready"
exec sleep 30
`,
			} {
				require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(script), 0755))
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("TEST_OCI_DIR", dir)
			t.Setenv("TEST_OCI_FAIL", strconv.FormatBool(test.fail))
			t.Setenv("FAST_SANDBOX_OCI_ENABLED", test.mode)
			t.Setenv("SANDBOX_TEMPLATE_SPEC", test.spec)
			t.Cleanup(func() {
				if raw, err := os.ReadFile(filepath.Join(dir, "pid")); err == nil {
					pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
					if err == nil {
						process, _ := os.FindProcess(pid)
						_ = process.Kill()
					}
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			output, err := exec.CommandContext(ctx, "sh", "entrypoint.sh", "sh", "-c", "echo runtime-started").CombinedOutput()
			if test.fail {
				require.Error(t, err)
				require.NotContains(t, string(output), "runtime-started")
			} else {
				require.NoError(t, err, string(output))
				require.Contains(t, string(output), "runtime-started")
			}
			_, err = os.Stat(filepath.Join(dir, "pid"))
			require.Equal(t, test.start, err == nil)
		})
	}
}
