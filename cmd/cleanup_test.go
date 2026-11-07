package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/dyung/grove/internal/config"
)

func TestResolveBackupPathFindsLegacyRelativePathInTrackedWorktree(t *testing.T) {
	repoRoot := t.TempDir()
	poolWorktree := t.TempDir()
	worktreeLink := filepath.Join(t.TempDir(), "worktree")
	if err := os.Symlink(poolWorktree, worktreeLink); err != nil {
		t.Fatal(err)
	}
	backupPath := filepath.Join(poolWorktree, ".venv.grove-bak")
	if err := os.Mkdir(backupPath, 0o755); err != nil {
		t.Fatal(err)
	}

	reg := &config.Registry{
		Worktrees: []config.Worktree{{Path: worktreeLink}},
	}
	got, err := resolveBackupPath(repoRoot, reg, config.BackupEntry{Path: ".venv.grove-bak"})
	if err != nil {
		t.Fatalf("resolveBackupPath: %v", err)
	}
	if got != filepath.Join(worktreeLink, ".venv.grove-bak") {
		t.Fatalf("resolved path = %q", got)
	}
}

func TestResolveBackupPathRejectsMissingBackup(t *testing.T) {
	repoRoot := t.TempDir()
	reg := &config.Registry{}
	_, err := resolveBackupPath(repoRoot, reg, config.BackupEntry{
		Path: filepath.Join(repoRoot, "missing.grove-bak"),
	})
	if err == nil {
		t.Fatal("expected missing backup to be rejected")
	}
}

func TestResolveBackupPathRejectsAmbiguousRelativePath(t *testing.T) {
	repoRoot := t.TempDir()
	worktreeRoot := t.TempDir()
	for _, root := range []string{repoRoot, worktreeRoot} {
		if err := os.Mkdir(filepath.Join(root, ".venv.grove-bak"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	reg := &config.Registry{
		Worktrees: []config.Worktree{{Path: worktreeRoot}},
	}
	if _, err := resolveBackupPath(repoRoot, reg, config.BackupEntry{Path: ".venv.grove-bak"}); err == nil {
		t.Fatal("expected ambiguous backup path to be rejected")
	}
}

func TestResolveBackupPathRejectsRelativePathTraversal(t *testing.T) {
	reg := &config.Registry{}
	if _, err := resolveBackupPath(t.TempDir(), reg, config.BackupEntry{Path: "../outside.grove-bak"}); err == nil {
		t.Fatal("expected relative path traversal to be rejected")
	}
}

func TestRemoveBackupVerifiesDeletion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "backup.grove-bak")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := removeBackup(path); err != nil {
		t.Fatalf("removeBackup: %v", err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("backup still exists or stat failed unexpectedly: %v", err)
	}
}

func TestRemoveBackupRejectsMissingPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.grove-bak")
	if err := removeBackup(path); err == nil {
		t.Fatal("expected missing backup to be rejected")
	}
}

func TestRunCleanupRemovesResolvedLegacyBackupAndRegistryEntry(t *testing.T) {
	repoRoot := t.TempDir()
	if err := exec.Command("git", "-C", repoRoot, "init").Run(); err != nil {
		t.Fatal(err)
	}
	poolWorktree := t.TempDir()
	worktreeLink := filepath.Join(repoRoot, "worktree")
	if err := os.Symlink(poolWorktree, worktreeLink); err != nil {
		t.Fatal(err)
	}
	backupPath := filepath.Join(poolWorktree, ".venv.grove-bak")
	if err := os.Mkdir(backupPath, 0o755); err != nil {
		t.Fatal(err)
	}

	reg := &config.Registry{
		Worktrees: []config.Worktree{{Path: worktreeLink}},
		Backups:   []config.BackupEntry{{Path: ".venv.grove-bak"}},
	}
	if err := reg.Save(repoRoot); err != nil {
		t.Fatal(err)
	}

	previousDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(repoRoot); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupYes = false
		if err := os.Chdir(previousDir); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	})
	cleanupYes = true

	if err := runCleanup(nil, nil); err != nil {
		t.Fatalf("runCleanup: %v", err)
	}
	if _, err := os.Lstat(backupPath); !os.IsNotExist(err) {
		t.Fatalf("backup still exists or stat failed unexpectedly: %v", err)
	}
	loaded, err := config.Load(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Backups) != 0 {
		t.Fatalf("registry retained %d backups, want 0", len(loaded.Backups))
	}
}

func TestRunCleanupRetainsMissingBackupRegistryEntry(t *testing.T) {
	repoRoot := t.TempDir()
	if err := exec.Command("git", "-C", repoRoot, "init").Run(); err != nil {
		t.Fatal(err)
	}
	missingPath := filepath.Join(repoRoot, "missing.grove-bak")
	reg := &config.Registry{
		Backups: []config.BackupEntry{{Path: missingPath}},
	}
	if err := reg.Save(repoRoot); err != nil {
		t.Fatal(err)
	}

	previousDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(repoRoot); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupYes = false
		if err := os.Chdir(previousDir); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	})
	cleanupYes = true

	if err := runCleanup(nil, nil); err != nil {
		t.Fatalf("runCleanup: %v", err)
	}
	loaded, err := config.Load(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Backups) != 1 || loaded.Backups[0].Path != missingPath {
		t.Fatalf("missing backup registry entry was not retained: %+v", loaded.Backups)
	}
}
