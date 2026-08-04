package janitor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	fastletnetwork "fast-sandbox/internal/fastlet/network"
	"fast-sandbox/internal/nodecleanup"
)

type LinuxNetworkBackend struct {
	stateRoot      string
	driver         fastletnetwork.Driver
	processCleaner nodecleanup.RuntimeProcessCleaner
}

func NewLinuxNetworkBackend(
	stateRoot string,
	driver fastletnetwork.Driver,
	processCleaners ...nodecleanup.RuntimeProcessCleaner,
) *LinuxNetworkBackend {
	backend := &LinuxNetworkBackend{stateRoot: stateRoot, driver: driver}
	if len(processCleaners) > 0 {
		backend.processCleaner = processCleaners[0]
	}
	return backend
}

func (*LinuxNetworkBackend) Name() ResourceBackend { return BackendLinuxNetwork }

func (b *LinuxNetworkBackend) Scan(ctx context.Context) ([]ResourceIdentity, error) {
	if b.stateRoot == "" || b.driver == nil {
		return nil, errors.New("Linux network state root and driver are required")
	}
	entries, err := os.ReadDir(b.stateRoot)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var resources []ResourceIdentity
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		podUID := entry.Name()
		store := fastletnetwork.NewFileStateStore(filepath.Join(b.stateRoot, podUID))
		slots, err := store.LoadAll(ctx)
		if err != nil {
			return nil, fmt.Errorf("load network state for Pod %s: %w", podUID, err)
		}
		for _, slot := range slots {
			if slot.OwnerPodUID != podUID {
				return nil, fmt.Errorf("network slot %s owner Pod UID does not match its state directory", slot.ID)
			}
			resources = append(resources, networkResource(slot))
		}
	}
	return resources, nil
}

func (b *LinuxNetworkBackend) Cleanup(ctx context.Context, expected ResourceIdentity) error {
	if b.stateRoot == "" || b.driver == nil {
		return errors.New("Linux network state root and driver are required")
	}
	if expected.NetworkStatePodUID == "" || expected.NetworkSlotID == "" {
		return errors.New("network resource is missing Pod UID or slot ID")
	}
	store := fastletnetwork.NewFileStateStore(filepath.Join(b.stateRoot, expected.NetworkStatePodUID))
	slots, err := store.LoadAll(ctx)
	if err != nil {
		return err
	}
	var current *fastletnetwork.Slot
	for _, slot := range slots {
		if slot.ID == expected.NetworkSlotID {
			current = slot
			break
		}
	}
	if current == nil {
		return nil
	}
	if !sameResourceFence(expected, networkResource(current)) {
		return errors.New("network slot identity changed before cleanup")
	}
	if current.Owner.ResidualProcess != "" {
		if b.processCleaner == nil {
			return fmt.Errorf("clean %s process for sandbox %s: node process cleaner is not configured", current.Owner.ResidualProcess, current.Owner.SandboxUID)
		}
		if err := b.processCleaner.EnsureRuntimeProcessesAbsent(ctx, current.Owner.ResidualProcess, current.Owner.SandboxUID); err != nil {
			// Keep the durable network record so a later Janitor scan can retry.
			return fmt.Errorf("clean %s process for sandbox %s: %w", current.Owner.ResidualProcess, current.Owner.SandboxUID, err)
		}
	}
	if err := b.driver.Destroy(ctx, current); err != nil {
		return err
	}
	if err := store.Delete(ctx, current.ID); err != nil {
		return err
	}
	_ = os.Remove(store.Root())
	return nil
}

func networkResource(slot *fastletnetwork.Slot) ResourceIdentity {
	if slot == nil {
		return ResourceIdentity{}
	}
	return ResourceIdentity{
		Backend: BackendLinuxNetwork, ResourceID: slot.OwnerPodUID + "/" + slot.ID,
		FastletPodUID: slot.OwnerPodUID, FastletPodName: slot.OwnerPodName, FastletPodNamespace: slot.OwnerNamespace,
		SandboxUID: slot.Owner.SandboxUID, SandboxName: slot.Owner.SandboxName, SandboxNamespace: slot.Owner.SandboxNamespace,
		InstanceGeneration: slot.Owner.InstanceGeneration, AssignmentAttempt: slot.Owner.AssignmentAttempt,
		CreatedAt: slot.CreatedAt, NetworkSlotID: slot.ID, NetworkStatePodUID: slot.OwnerPodUID,
		ResidualProcess: slot.Owner.ResidualProcess,
	}
}
