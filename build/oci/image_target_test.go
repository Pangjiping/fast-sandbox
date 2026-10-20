package oci_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFastletImageBuildDoesNotRequireHostCompilers(t *testing.T) {
	root := t.TempDir()
	makefile, err := os.ReadFile("../../Makefile")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, "Makefile"), makefile, 0600))
	bin := filepath.Join(root, "bin")
	require.NoError(t, os.MkdirAll(bin, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$TEST_IMAGE_LOG\"\n"), 0755))
	log := filepath.Join(root, "commands")
	cmd := exec.Command("make", "images", "COMPONENT=fastlet", "GO="+filepath.Join(bin, "missing-go"), "DEBUG=1", "FASTLET_IMAGE=opensandbox/fsb-fastlet:test")
	cmd.Dir = root
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "GOPROXY=") {
			cmd.Env = append(cmd.Env, value)
		}
	}
	cmd.Env = append(cmd.Env, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "TEST_IMAGE_LOG="+log)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
	raw, err := os.ReadFile(log)
	require.NoError(t, err)
	require.Equal(t, "build\n--platform\nlinux/amd64\n--build-arg\nGOPROXY=https://proxy.golang.org,direct\n--build-arg\nDEBUG=1\n-t\nopensandbox/fsb-fastlet:test\n-f\nbuild/Dockerfile.fastlet\n.\n", string(raw))
	require.NoDirExists(t, filepath.Join(root, ".build"))
}
