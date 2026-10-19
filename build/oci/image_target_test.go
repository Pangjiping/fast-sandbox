package oci_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOCIImageTargetBuildsToolsAndRuntime(t *testing.T) {
	root := t.TempDir()
	makefile, err := os.ReadFile("../../Makefile")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, "Makefile"), makefile, 0600))
	bin := filepath.Join(root, "bin")
	require.NoError(t, os.MkdirAll(bin, 0755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "scripts"), 0755))
	script := `#!/bin/sh
set -eu
echo native-tools >> "$TEST_IMAGE_LOG"
mkdir -p .build/oci-tools/bin .build/oci-tools/lib .build/linux-amd64
for tool in overlaybd-import-raw overlaybd-ublkd overlaybd-create overlaybd-apply overlaybd-commit overlaybd-resize; do
 touch ".build/oci-tools/bin/$tool"; chmod 755 ".build/oci-tools/bin/$tool"
done
touch .build/oci-tools/lib/libext2fs.so.2 .build/linux-amd64/convertor
chmod 755 .build/linux-amd64/convertor
`
	require.NoError(t, os.WriteFile(filepath.Join(root, "scripts/build-oci-tools.sh"), []byte(script), 0755))
	for _, tool := range []string{"test-go", "docker"} {
		require.NoError(t, os.WriteFile(filepath.Join(bin, tool), []byte("#!/bin/sh\necho "+tool+" \"$@\" >> \"$TEST_IMAGE_LOG\"\n"), 0755))
	}
	log := filepath.Join(root, "commands")
	for i := 0; i < 2; i++ {
		cmd := exec.Command("make", "images-oci", "GO="+filepath.Join(bin, "test-go"), "GOPROXY=off", "OCI_FASTLET_IMAGE=opensandbox/fsb-fastlet:test-oci")
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "TEST_IMAGE_LOG="+log)
		output, err := cmd.CombinedOutput()
		require.NoError(t, err, string(output))
	}
	raw, err := os.ReadFile(log)
	require.NoError(t, err)
	commands := string(raw)
	require.Equal(t, 1, strings.Count(commands, "native-tools\n"), "warm builds reuse staged native tools")
	for _, binary := range []string{"fastlet", "sandbox-init", "fsb-vm"} {
		require.Contains(t, commands, "./cmd/"+binary)
	}
	require.Equal(t, 2, strings.Count(commands, "docker build --build-arg COMPONENT=fastlet -t opensandbox/fsb-fastlet:test-oci -f build/Dockerfile.oci ."))
}
