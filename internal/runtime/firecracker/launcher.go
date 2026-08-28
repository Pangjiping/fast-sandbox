package firecracker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// firecrackerIDLimit matches the NodeJanitor residual-process matcher
// (internal/nodecleanup/process.go): firecracker is launched with
// --id <sandboxID[:32]> so the node daemon can identify and clean up a VMM
// that outlived both the Sandbox object and the Fastlet Pod.
const firecrackerIDLimit = 32

// Process is the host-side handle of one Firecracker microVM process.
type Process interface {
	PID() int
	Kill() error
	Wait() error
}

// ProcessRunner starts Firecracker processes. It is injectable so tests can
// substitute a fake process.
type ProcessRunner interface {
	Start(ctx context.Context, name string, args []string, logPath string) (Process, error)
}

// WorkingDirRunner starts Firecracker processes with a fixed working
// directory, so relative device paths baked in a snapshot vmstate resolve
// per instance.
type WorkingDirRunner interface {
	StartInDir(ctx context.Context, workingDir, name string, args []string, logPath string) (Process, error)
}

// ExecProcessRunner starts Firecracker with exec.Command. The VM deliberately
// outlives the caller context: only Kill terminates it. The process stdout
// and stderr are appended to logPath so a failed boot is diagnosable.
type ExecProcessRunner struct{}

// Start launches the process without binding its lifetime to ctx.
func (ExecProcessRunner) Start(_ context.Context, name string, args []string, logPath string) (Process, error) {
	return ExecProcessRunner{}.StartInDir(context.Background(), "", name, args, logPath)
}

// StartInDir launches the process with the given working directory.
func (ExecProcessRunner) StartInDir(_ context.Context, workingDir, name string, args []string, logPath string) (Process, error) {
	command := exec.Command(name, args...)
	if workingDir != "" {
		command.Dir = workingDir
	}
	if logPath != "" {
		logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
		if err != nil {
			return nil, err
		}
		command.Stdout = logFile
		command.Stderr = logFile
	}
	if err := command.Start(); err != nil {
		return nil, err
	}
	return &execProcess{command: command}, nil
}

type execProcess struct {
	command *exec.Cmd
}

func (p *execProcess) PID() int {
	if p.command.Process == nil {
		return 0
	}
	return p.command.Process.Pid
}

func (p *execProcess) Kill() error {
	if p.command.Process == nil {
		return nil
	}
	return p.command.Process.Kill()
}

func (p *execProcess) Wait() error {
	if p.command.Process == nil {
		return nil
	}
	return p.command.Wait()
}

// launchConfig is the immutable per-Sandbox Firecracker launch plan.
type launchConfig struct {
	// BinaryPath is the host path of the firecracker binary.
	BinaryPath string
	// SandboxID is the full Sandbox identity; the argv id is truncated.
	SandboxID string
	// APIAddress is the host path of the per-Sandbox API Unix socket.
	APIAddress string
	// WorkingDir is the process working directory. Snapshot restore opens
	// the block device paths baked in the vmstate relative to the
	// Firecracker process cwd (design: each instance resolves "rootfs.img"
	// to its own reflink copy in its state directory).
	WorkingDir string
	// ChrootBase is the optional jailer working root. Empty disables the jailer.
	ChrootBase string
	// LogPath receives the firecracker stdout/stderr (empty discards output).
	LogPath string
}

// buildArgv produces the Firecracker command line. The binary name must stay
// "firecracker" and the --id value must match the NodeJanitor matcher.
func (c launchConfig) buildArgv() []string {
	arguments := []string{
		"--id", c.truncatedID(),
		"--api-sock", c.APIAddress,
	}
	if c.ChrootBase != "" {
		arguments = append(arguments, "--chroot-base", c.ChrootBase)
	}
	return arguments
}

// truncatedID returns the NodeJanitor-compatible VM id.
func (c launchConfig) truncatedID() string {
	if len(c.SandboxID) > firecrackerIDLimit {
		return c.SandboxID[:firecrackerIDLimit]
	}
	return c.SandboxID
}

// launch invokes the firecracker binary with the launch configuration.
func launch(ctx context.Context, runner ProcessRunner, config launchConfig) (Process, error) {
	if strings.TrimSpace(config.BinaryPath) == "" || strings.TrimSpace(config.SandboxID) == "" || strings.TrimSpace(config.APIAddress) == "" {
		return nil, fmt.Errorf("%w: firecracker binary, sandbox id, and API address are required", ErrInvalidConfig)
	}
	if !filepath.IsAbs(config.APIAddress) {
		return nil, fmt.Errorf("%w: firecracker API address must be an absolute path", ErrInvalidConfig)
	}
	if runnerInDir, ok := runner.(WorkingDirRunner); ok && config.WorkingDir != "" {
		return runnerInDir.StartInDir(ctx, config.WorkingDir, config.BinaryPath, config.buildArgv(), config.LogPath)
	}
	return runner.Start(ctx, config.BinaryPath, config.buildArgv(), config.LogPath)
}
