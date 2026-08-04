package nodecleanup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	runtimecatalog "fast-sandbox/internal/catalog/runtime"
)

const firecrackerIDLimit = 32

type processIdentity struct {
	PID       int
	StartTime string
	Kind      runtimecatalog.ResidualProcessKind
	SandboxID string
}

type processTable interface {
	List(runtimecatalog.ResidualProcessKind, string) ([]processIdentity, error)
	Signal(processIdentity, syscall.Signal) error
}

type HostProcessCleaner struct {
	table           processTable
	naturalExitWait time.Duration
	terminateWait   time.Duration
	killWait        time.Duration
	pollInterval    time.Duration
}

func NewHostProcessCleaner() *HostProcessCleaner {
	return &HostProcessCleaner{
		table:           &procProcessTable{root: "/proc", signal: syscall.Kill},
		naturalExitWait: 250 * time.Millisecond,
		terminateWait:   time.Second,
		killWait:        time.Second,
		pollInterval:    25 * time.Millisecond,
	}
}

func (c *HostProcessCleaner) EnsureRuntimeProcessesAbsent(
	ctx context.Context,
	kind runtimecatalog.ResidualProcessKind,
	sandboxID string,
) error {
	if kind != runtimecatalog.ResidualProcessFirecracker {
		return fmt.Errorf("unsupported residual process kind %q", kind)
	}
	if sandboxID == "" {
		return errors.New("sandbox ID is required")
	}
	absent, remaining, err := c.waitUntilAbsent(ctx, kind, sandboxID, c.naturalExitWait)
	if err != nil || absent {
		return err
	}
	for _, process := range remaining {
		if err := c.table.Signal(process, syscall.SIGTERM); err != nil {
			return fmt.Errorf("terminate %s process %d for sandbox %s: %w", kind, process.PID, sandboxID, err)
		}
	}
	absent, remaining, err = c.waitUntilAbsent(ctx, kind, sandboxID, c.terminateWait)
	if err != nil || absent {
		return err
	}
	for _, process := range remaining {
		if err := c.table.Signal(process, syscall.SIGKILL); err != nil {
			return fmt.Errorf("kill %s process %d for sandbox %s: %w", kind, process.PID, sandboxID, err)
		}
	}
	absent, remaining, err = c.waitUntilAbsent(ctx, kind, sandboxID, c.killWait)
	if err != nil {
		return err
	}
	if !absent {
		pids := make([]string, 0, len(remaining))
		for _, process := range remaining {
			pids = append(pids, strconv.Itoa(process.PID))
		}
		return fmt.Errorf("%s process still exists for sandbox %s after SIGKILL: pids=%s", kind, sandboxID, strings.Join(pids, ","))
	}
	return nil
}

func (c *HostProcessCleaner) waitUntilAbsent(
	ctx context.Context,
	kind runtimecatalog.ResidualProcessKind,
	sandboxID string,
	timeout time.Duration,
) (bool, []processIdentity, error) {
	deadline := time.Now().Add(timeout)
	for {
		processes, err := c.table.List(kind, sandboxID)
		if err != nil {
			return false, nil, err
		}
		if len(processes) == 0 {
			return true, nil, nil
		}
		if timeout <= 0 || !time.Now().Before(deadline) {
			return false, processes, nil
		}
		wait := c.pollInterval
		if remaining := time.Until(deadline); wait > remaining {
			wait = remaining
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false, processes, ctx.Err()
		case <-timer.C:
		}
	}
}

type procProcessTable struct {
	root   string
	signal func(int, syscall.Signal) error
}

func (t *procProcessTable) List(kind runtimecatalog.ResidualProcessKind, sandboxID string) ([]processIdentity, error) {
	entries, err := os.ReadDir(t.root)
	if err != nil {
		return nil, fmt.Errorf("list process table: %w", err)
	}
	var result []processIdentity
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		identity, match, err := t.readIdentity(pid, kind, sandboxID)
		if err != nil {
			if os.IsNotExist(err) || os.IsPermission(err) {
				continue
			}
			return nil, err
		}
		if match {
			result = append(result, identity)
		}
	}
	return result, nil
}

func (t *procProcessTable) Signal(process processIdentity, signal syscall.Signal) error {
	current, match, err := t.readIdentity(process.PID, process.Kind, process.SandboxID)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if !match || current.StartTime != process.StartTime {
		// The PID exited or was reused after discovery. Never signal the new
		// process; the following rescan decides whether the target remains.
		return nil
	}
	if err := t.signal(process.PID, signal); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

func (t *procProcessTable) readIdentity(
	pid int,
	kind runtimecatalog.ResidualProcessKind,
	sandboxID string,
) (processIdentity, bool, error) {
	base := filepath.Join(t.root, strconv.Itoa(pid))
	payload, err := os.ReadFile(filepath.Join(base, "cmdline"))
	if err != nil {
		return processIdentity{}, false, err
	}
	args := splitCmdline(payload)
	if !matchesRuntimeProcess(kind, sandboxID, args) {
		return processIdentity{}, false, nil
	}
	stat, err := os.ReadFile(filepath.Join(base, "stat"))
	if err != nil {
		return processIdentity{}, false, err
	}
	startTime, err := procStartTime(string(stat))
	if err != nil {
		return processIdentity{}, false, fmt.Errorf("read process %d identity: %w", pid, err)
	}
	return processIdentity{PID: pid, StartTime: startTime, Kind: kind, SandboxID: sandboxID}, true, nil
}

func splitCmdline(payload []byte) []string {
	trimmed := strings.TrimRight(string(payload), "\x00")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\x00")
}

func matchesRuntimeProcess(kind runtimecatalog.ResidualProcessKind, sandboxID string, args []string) bool {
	if kind != runtimecatalog.ResidualProcessFirecracker || len(args) == 0 || filepath.Base(args[0]) != "firecracker" {
		return false
	}
	expectedID := sandboxID
	if len(expectedID) > firecrackerIDLimit {
		expectedID = expectedID[:firecrackerIDLimit]
	}
	for i := 1; i+1 < len(args); i++ {
		if args[i] == "--id" && args[i+1] == expectedID {
			return true
		}
	}
	return false
}

func procStartTime(stat string) (string, error) {
	end := strings.LastIndex(stat, ")")
	if end < 0 || end+1 >= len(stat) {
		return "", errors.New("malformed /proc stat")
	}
	fields := strings.Fields(stat[end+1:])
	// Field 3 (state) is fields[0]; starttime is Linux proc field 22.
	if len(fields) <= 19 {
		return "", errors.New("short /proc stat")
	}
	return fields[19], nil
}
