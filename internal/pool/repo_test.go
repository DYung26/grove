package pool

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestEnsureDependencyDirVolumeReconcilesExistingSubvolume(t *testing.T) {
	if _, err := exec.LookPath("rsync"); err != nil {
		t.Skip("rsync is required")
	}

	binDir := t.TempDir()
	fakeBtrfs := filepath.Join(binDir, "btrfs")
	if err := os.WriteFile(fakeBtrfs, []byte("#!/bin/sh\n[ \"$1 $2\" = \"subvolume show\" ]\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	mountPoint := t.TempDir()
	worktreeRoot := t.TempDir()
	subvolume := filepath.Join(mountPoint, "demo-main-node_modules")
	if err := os.MkdirAll(subvolume, 0o755); err != nil {
		t.Fatal(err)
	}

	sibling := filepath.Join(worktreeRoot, "node_modules")
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sibling, "package.json"), []byte("source"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, backup, err := EnsureDependencyDirVolume(mountPoint, "demo", "main", worktreeRoot, "node_modules")
	if err != nil {
		t.Fatal(err)
	}
	if backup != sibling+".grove-bak" {
		t.Fatalf("backup = %q, want %q", backup, sibling+".grove-bak")
	}

	linkTarget, err := filepath.EvalSymlinks(sibling)
	if err != nil {
		t.Fatal(err)
	}
	if linkTarget != subvolume {
		t.Fatalf("dependency symlink resolves to %q, want %q", linkTarget, subvolume)
	}
	contents, err := os.ReadFile(filepath.Join(subvolume, "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "source" {
		t.Fatalf("reconciled contents = %q, want %q", contents, "source")
	}

	_, backup, err = EnsureDependencyDirVolume(mountPoint, "demo", "main", worktreeRoot, "node_modules")
	if err != nil {
		t.Fatal(err)
	}
	if backup != "" {
		t.Fatalf("second reconciliation returned backup %q, want empty", backup)
	}
}

func TestDeleteSubvolumeForRollbackFallsBackToEmptySubvolumeRemoval(t *testing.T) {
	binDir := t.TempDir()
	fakeBtrfs := filepath.Join(binDir, "btrfs")
	if err := os.WriteFile(fakeBtrfs, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	mountPoint := t.TempDir()
	subvolume := filepath.Join(mountPoint, "rollback-subvolume")
	if err := os.MkdirAll(filepath.Join(subvolume, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subvolume, "nested", "file"), []byte("disposable"), 0o644); err != nil {
		t.Fatal(err)
	}

	linkPath := filepath.Join(t.TempDir(), "worktree")
	if err := os.Symlink(subvolume, linkPath); err != nil {
		t.Fatal(err)
	}

	if err := DeleteSubvolumeForRollback(linkPath, mountPoint); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(linkPath); !os.IsNotExist(err) {
		t.Fatalf("rollback symlink still exists: %v", err)
	}
	if _, err := os.Lstat(subvolume); !os.IsNotExist(err) {
		t.Fatalf("rollback subvolume still exists: %v", err)
	}
}

func TestDependencyVolumeNameDistinguishesNestedSameBasename(t *testing.T) {
	daemon := DependencyVolumeName("demo", "main", "daemon/node_modules")
	extension := DependencyVolumeName("demo", "main", "extension/node_modules")
	if daemon == extension {
		t.Fatalf("nested dependency volume names collide: %q", daemon)
	}
	if daemon != DependencyVolumeName("demo", "main", "daemon/node_modules") {
		t.Fatal("dependency volume name is not deterministic")
	}
	if LegacyDependencyVolumeName("demo", "main", "daemon/node_modules") != "demo-main-node_modules" {
		t.Fatalf("legacy name changed unexpectedly")
	}
}
