package nodecleanup

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	runtimecatalog "fast-sandbox/internal/catalog/runtime"

	"github.com/stretchr/testify/require"
)

func TestMatchesFirecrackerProcessUsesExactKataTruncatedID(t *testing.T) {
	sandboxID := "4f65cc43-8b14-4579-847b-fc62476e8252"
	require.True(t, matchesRuntimeProcess(runtimecatalog.ResidualProcessFirecracker, sandboxID, []string{
		"/firecracker", "--id", "4f65cc43-8b14-4579-847b-fc62476e", "--config-file", "/fcConfig.json",
	}))
	require.False(t, matchesRuntimeProcess(runtimecatalog.ResidualProcessFirecracker, sandboxID, []string{
		"/firecracker", "--id", "4f65cc43-8b14-4579-847b-fc62476f",
	}))
	require.False(t, matchesRuntimeProcess(runtimecatalog.ResidualProcessFirecracker, sandboxID, []string{
		"/usr/bin/not-firecracker", "--id", "4f65cc43-8b14-4579-847b-fc62476e",
	}))
}

func TestProcProcessTableFindsOnlyMatchingFirecracker(t *testing.T) {
	root := t.TempDir()
	writeProcFixture(t, root, 101, "/firecracker\x00--id\x004f65cc43-8b14-4579-847b-fc62476e\x00", "101 (firecracker) S "+statFields(19, "999"))
	writeProcFixture(t, root, 102, "/firecracker\x00--id\x00different\x00", "102 (firecracker) S "+statFields(19, "1000"))
	writeProcFixture(t, root, 103, "/opt/kata/bin/containerd-shim-kata-v2\x00-id\x004f65cc43-8b14-4579-847b-fc62476e8252\x00", "103 (shim) S "+statFields(19, "1001"))
	table := &procProcessTable{root: root, signal: func(int, syscall.Signal) error { return nil }}

	processes, err := table.List(runtimecatalog.ResidualProcessFirecracker, "4f65cc43-8b14-4579-847b-fc62476e8252")

	require.NoError(t, err)
	require.Equal(t, []processIdentity{{
		PID: 101, StartTime: "999", Kind: runtimecatalog.ResidualProcessFirecracker,
		SandboxID: "4f65cc43-8b14-4579-847b-fc62476e8252",
	}}, processes)
}

func TestHostProcessCleanerTerminatesResidualProcess(t *testing.T) {
	table := newFakeProcessTable(41)
	table.removeOnSignal = syscall.SIGTERM
	cleaner := testHostProcessCleaner(table)

	err := cleaner.EnsureRuntimeProcessesAbsent(context.Background(), runtimecatalog.ResidualProcessFirecracker, "sandbox")

	require.NoError(t, err)
	require.Equal(t, []syscall.Signal{syscall.SIGTERM}, table.signals)
}

func TestHostProcessCleanerKillsProcessWhichIgnoresTerminate(t *testing.T) {
	table := newFakeProcessTable(42)
	table.removeOnSignal = syscall.SIGKILL
	cleaner := testHostProcessCleaner(table)

	err := cleaner.EnsureRuntimeProcessesAbsent(context.Background(), runtimecatalog.ResidualProcessFirecracker, "sandbox")

	require.NoError(t, err)
	require.Equal(t, []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL}, table.signals)
}

func TestHostProcessCleanerFailsWhenProcessSurvivesKill(t *testing.T) {
	table := newFakeProcessTable(43)
	cleaner := testHostProcessCleaner(table)

	err := cleaner.EnsureRuntimeProcessesAbsent(context.Background(), runtimecatalog.ResidualProcessFirecracker, "sandbox")

	require.ErrorContains(t, err, "still exists")
	require.Equal(t, []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL}, table.signals)
}

func TestProcProcessTableDoesNotSignalReusedPID(t *testing.T) {
	root := t.TempDir()
	writeProcFixture(t, root, 104, "/firecracker\x00--id\x00sandbox\x00", "104 (firecracker) S "+statFields(19, "111"))
	var signals []syscall.Signal
	table := &procProcessTable{root: root, signal: func(_ int, signal syscall.Signal) error {
		signals = append(signals, signal)
		return nil
	}}
	processes, err := table.List(runtimecatalog.ResidualProcessFirecracker, "sandbox")
	require.NoError(t, err)
	require.Len(t, processes, 1)
	writeProcFixture(t, root, 104, "/firecracker\x00--id\x00sandbox\x00", "104 (firecracker) S "+statFields(19, "222"))

	require.NoError(t, table.Signal(processes[0], syscall.SIGKILL))
	require.Empty(t, signals)
}

func testHostProcessCleaner(table processTable) *HostProcessCleaner {
	return &HostProcessCleaner{
		table: table, naturalExitWait: 0, terminateWait: 0, killWait: 0,
		pollInterval: time.Microsecond,
	}
}

type fakeProcessTable struct {
	process        *processIdentity
	removeOnSignal syscall.Signal
	signals        []syscall.Signal
}

func newFakeProcessTable(pid int) *fakeProcessTable {
	return &fakeProcessTable{process: &processIdentity{
		PID: pid, StartTime: "1", Kind: runtimecatalog.ResidualProcessFirecracker, SandboxID: "sandbox",
	}}
}

func (t *fakeProcessTable) List(kind runtimecatalog.ResidualProcessKind, sandboxID string) ([]processIdentity, error) {
	if t.process == nil {
		return nil, nil
	}
	if t.process.Kind != kind || t.process.SandboxID != sandboxID {
		return nil, nil
	}
	return []processIdentity{*t.process}, nil
}

func (t *fakeProcessTable) Signal(_ processIdentity, signal syscall.Signal) error {
	t.signals = append(t.signals, signal)
	if signal == t.removeOnSignal {
		t.process = nil
	}
	return nil
}

func writeProcFixture(t *testing.T, root string, pid int, cmdline, stat string) {
	t.Helper()
	dir := filepath.Join(root, fmt.Sprint(pid))
	require.NoError(t, os.MkdirAll(dir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cmdline"), []byte(cmdline), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0644))
}

func statFields(count int, last string) string {
	fields := make([]byte, 0, count*2)
	for i := 0; i < count-1; i++ {
		fields = append(fields, '0', ' ')
	}
	fields = append(fields, last...)
	return string(fields)
}
