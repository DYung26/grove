package pool

import (
	"os"
	"path/filepath"
	"testing"
)

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

func TestEnsureWorktreeVolumeCleansPartialSubvolumeAfterRsyncFailure(t *testing.T) {
	binDir := t.TempDir()
	fakeBtrfs := filepath.Join(binDir, "btrfs")
	btrfsScript := `#!/bin/sh
if [ "$1 $2" = "subvolume create" ]; then mkdir -p "$3"; exit 0; fi
if [ "$1 $2" = "subvolume delete" ]; then rm -rf "$3"; exit 0; fi
exit 1
`
	if err := os.WriteFile(fakeBtrfs, []byte(btrfsScript), 0o755); err != nil {
		t.Fatal(err)
	}
	fakeRsync := filepath.Join(binDir, "rsync")
	rsyncScript := `#!/bin/sh
echo "rsync: write failed: No space left on device (28)" >&2
exit 11
`
	if err := os.WriteFile(fakeRsync, []byte(rsyncScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	mountPoint := t.TempDir()
	worktree := filepath.Join(t.TempDir(), "worktree")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".dockerignore"), []byte("source"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := EnsureWorktreeVolume(mountPoint, "repo", "feature", worktree)
	if err == nil {
		t.Fatal("EnsureWorktreeVolume() succeeded despite simulated ENOSPC")
	}
	if _, statErr := os.Stat(filepath.Join(mountPoint, "repo-feature")); !os.IsNotExist(statErr) {
		t.Fatalf("partial pool subvolume remains: %v", statErr)
	}
	contents, readErr := os.ReadFile(filepath.Join(worktree, ".dockerignore"))
	if readErr != nil || string(contents) != "source" {
		t.Fatalf("original worktree was not preserved: contents=%q error=%v", contents, readErr)
	}
}
