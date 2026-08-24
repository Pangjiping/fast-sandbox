package firecracker

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type fakeProcess struct {
	pid    int
	killed bool
}

func (f *fakeProcess) PID() int { return f.pid }
func (f *fakeProcess) Kill() error {
	f.killed = true
	return nil
}
func (f *fakeProcess) Wait() error { return nil }

type fakeProcessRunner struct {
	started [][]string
	err     error
}

func (f *fakeProcessRunner) Start(_ context.Context, name string, args []string, _ string) (Process, error) {
	f.started = append(f.started, append([]string{name}, args...))
	if f.err != nil {
		return nil, f.err
	}
	return &fakeProcess{pid: 4242}, nil
}

func TestBuildArgvTruncatesID(t *testing.T) {
	longID := strings.Repeat("s", 40)
	argv := (launchConfig{SandboxID: longID, APIAddress: "/var/lib/fast-sandbox/api.sock"}).buildArgv()
	require.Equal(t, []string{"--id", strings.Repeat("s", 32), "--api-sock", "/var/lib/fast-sandbox/api.sock"}, argv)

	shortID := "sandbox-1"
	argv = (launchConfig{SandboxID: shortID, APIAddress: "/var/lib/fast-sandbox/api.sock"}).buildArgv()
	require.Contains(t, argv, "--id")
	require.Equal(t, shortID, argv[argvIndex(argv, "--id")+1])
}

func TestBuildArgvWithChrootBase(t *testing.T) {
	argv := (launchConfig{SandboxID: "sandbox-1", APIAddress: "/run/api.sock", ChrootBase: "/var/lib/fast-sandbox/jails"}).buildArgv()
	require.Contains(t, argv, "--chroot-base")
	require.Equal(t, "/var/lib/fast-sandbox/jails", argv[argvIndex(argv, "--chroot-base")+1])
}

func TestLaunchValidation(t *testing.T) {
	runner := &fakeProcessRunner{}
	ctx := context.Background()

	_, err := launch(ctx, runner, launchConfig{SandboxID: "sandbox-1", APIAddress: "/run/api.sock"})
	require.ErrorIs(t, err, ErrInvalidConfig)

	_, err = launch(ctx, runner, launchConfig{BinaryPath: "/usr/local/bin/firecracker", APIAddress: "relative.sock"})
	require.ErrorIs(t, err, ErrInvalidConfig)
	require.Empty(t, runner.started)
}

func TestLaunchInvokesRunner(t *testing.T) {
	runner := &fakeProcessRunner{}
	ctx := context.Background()

	process, err := launch(ctx, runner, launchConfig{BinaryPath: "/usr/local/bin/firecracker", SandboxID: "sandbox-1", APIAddress: "/run/api.sock"})
	require.NoError(t, err)
	require.Equal(t, 4242, process.PID())
	require.Len(t, runner.started, 1)
	require.Equal(t, "/usr/local/bin/firecracker", runner.started[0][0])
	require.Contains(t, runner.started[0], "--api-sock")
}

func argvIndex(argv []string, flag string) int {
	for index, value := range argv {
		if value == flag {
			return index
		}
	}
	return -1
}
