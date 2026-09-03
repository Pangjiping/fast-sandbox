package server

import (
	"context"
	"testing"

	agentstate "fast-sandbox/internal/runtime/firecracker/agent/state"

	"github.com/stretchr/testify/require"
)

// TestServiceHealthDartProbe wires the node-local DART state into Health:
// DartUp follows the probe, and the agent's own OK stays independent of the
// DART daemon (a broken gateway keeps pulls on the direct-S3 fallback).
func TestServiceHealthDartProbe(t *testing.T) {
	stateRoot := t.TempDir()
	state, err := agentstate.New(stateRoot)
	require.NoError(t, err)
	t.Cleanup(func() { _ = state.Close() })

	probed := true
	service := NewService(newFakePuller(), state, stateRoot, WithDARTProbe(func() bool { return probed }))
	health, err := service.Health(context.Background())
	require.NoError(t, err)
	require.True(t, health.OK)
	require.True(t, health.DartUp, "Health must reflect the DART probe")

	probed = false
	health, err = service.Health(context.Background())
	require.NoError(t, err)
	require.True(t, health.OK, "agent health stays green when DART is down")
	require.False(t, health.DartUp)
}

func TestServiceHealthWithoutDARTProbe(t *testing.T) {
	stateRoot := t.TempDir()
	state, err := agentstate.New(stateRoot)
	require.NoError(t, err)
	t.Cleanup(func() { _ = state.Close() })

	service := NewService(newFakePuller(), state, stateRoot)
	health, err := service.Health(context.Background())
	require.NoError(t, err)
	require.False(t, health.DartUp, "no DART probe configured = stage-1 local mode")
}
