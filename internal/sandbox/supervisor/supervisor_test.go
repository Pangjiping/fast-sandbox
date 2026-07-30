package supervisor

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	infracatalog "fast-sandbox/internal/catalog/infra"

	"github.com/stretchr/testify/require"
)

func TestSupervisorPreservesUserExitCodeAndStartsComponentConcurrently(t *testing.T) {
	root := t.TempDir()
	componentMarker := filepath.Join(root, "component")
	userMarker := filepath.Join(root, "user")
	config := Config{Version: ConfigVersion, SandboxUID: "uid-a", Components: []Component{{
		Name: "component", Command: "/bin/sh",
		Args:          []string{"-c", "sleep 0.2; touch " + componentMarker + "; exec sleep 30"},
		RestartPolicy: infracatalog.RestartNever,
		Readiness:     Readiness{Type: infracatalog.ProbeTCP, Address: "127.0.0.1:44772"},
	}}}
	supervisor := NewSupervisor(io.Discard, io.Discard)
	start := time.Now()
	exitCode, err := supervisor.Run(context.Background(), config, []string{"/bin/sh", "-c", "touch " + userMarker + "; exit 7"})
	require.NoError(t, err)
	require.Equal(t, 7, exitCode)
	require.Less(t, time.Since(start), 2*time.Second)
	require.FileExists(t, userMarker)
	_, statErr := os.Stat(componentMarker)
	require.ErrorIs(t, statErr, os.ErrNotExist, "component readiness must not gate user startup")
}

func TestComponentStartFailureDoesNotFailUserProcess(t *testing.T) {
	config := Config{Version: ConfigVersion, SandboxUID: "uid-a", Components: []Component{{
		Name: "missing", Command: "/definitely/missing/infra-component",
		RestartPolicy: infracatalog.RestartNever,
		Readiness:     Readiness{Type: infracatalog.ProbeTCP, Address: "127.0.0.1:44772"},
	}}}
	code, err := NewSupervisor(io.Discard, io.Discard).Run(context.Background(), config, []string{"/bin/sh", "-c", "exit 0"})
	require.NoError(t, err)
	require.Zero(t, code)
}

func TestConfigRejectsIncompleteAndDuplicateComponents(t *testing.T) {
	require.Error(t, (Config{}).Validate())
	require.ErrorContains(t, (Config{
		Version: ConfigVersion, SandboxUID: "uid-a",
		Components: []Component{{Name: "execd"}},
	}).Validate(), "name and command")
	require.ErrorContains(t, (Config{
		Version: ConfigVersion, SandboxUID: "uid-a",
		Components: []Component{
			{Name: "execd", Command: "/bin/true"},
			{Name: "execd", Command: "/bin/true"},
		},
	}).Validate(), "duplicate")
}

func TestRestartPolicy(t *testing.T) {
	require.True(t, shouldRestart(infracatalog.RestartAlways, nil))
	require.False(t, shouldRestart(infracatalog.RestartOnFailure, nil))
	require.True(t, shouldRestart(infracatalog.RestartOnFailure, errors.New("failed")))
	require.False(t, shouldRestart(infracatalog.RestartNever, errors.New("failed")))
}

func TestUserProcessAttributesPreserveOriginalOCICredential(t *testing.T) {
	attributes := userProcessAttributes(&UserCredential{UID: 1000, GID: 1001, AdditionalGIDs: []uint32{10, 20}})
	require.NotNil(t, attributes.Credential)
	require.Equal(t, uint32(1000), attributes.Credential.Uid)
	require.Equal(t, uint32(1001), attributes.Credential.Gid)
	require.Equal(t, []uint32{10, 20}, attributes.Credential.Groups)
}
