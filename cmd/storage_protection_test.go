package cmd

import "testing"

func TestStorageCommandClassification(t *testing.T) {
	if !isMutatingCommand(createCmd) || !isStorageIntensiveCommand(createCmd) {
		t.Fatal("create must be a storage-intensive mutation")
	}
	if isMutatingCommand(poolStatusCmd) {
		t.Fatal("pool status must remain read-only")
	}
	if !isRecoveryCommand(cleanCmd, nil) || !isRecoveryCommand(cleanupCmd, nil) || !isRecoveryCommand(pruneCmd, nil) {
		t.Fatal("cleanup and recovery commands must bypass capacity thresholds")
	}
	if allowsReadOnlyPool(cleanCmd) || allowsReadOnlyPool(removeCmd) {
		t.Fatal("worktree deletion must not proceed against a read-only pool")
	}
	if !allowsReadOnlyPool(cleanupCmd) || !allowsReadOnlyPool(pruneCmd) || !allowsReadOnlyPool(poolRepairCmd) {
		t.Fatal("backing-only cleanup and pool repair must remain available for a read-only pool")
	}
}

func TestPoolGrowthIsRecoveryOnlyForPositiveDelta(t *testing.T) {
	if !isRecoveryCommand(poolResizeCmd, []string{"+2"}) {
		t.Fatal("pool growth must be available as capacity recovery")
	}
	if isRecoveryCommand(poolResizeCmd, []string{"-2"}) || isRecoveryCommand(poolResizeCmd, []string{"20"}) {
		t.Fatal("pool shrink and absolute resize must not bypass capacity thresholds")
	}
}
