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
