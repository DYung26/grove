package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dyung/grove/internal/config"
)

func TestStaleWorktreesKeepsExistingDirectoryEvenIfBranchIsMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worktree")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}

	stale, err := staleWorktrees(&config.Registry{Worktrees: []config.Worktree{{
		Name: "feature", Path: path, Branch: "already-deleted",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 0 {
		t.Fatalf("staleWorktrees() returned %d entries for an existing directory; want 0", len(stale))
	}
}

func TestStaleWorktreesFindsMissingDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing-worktree")
	stale, err := staleWorktrees(&config.Registry{Worktrees: []config.Worktree{{
		Name: "feature", Path: path, Branch: "feature",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 1 || stale[0].Name != "feature" {
		t.Fatalf("staleWorktrees() = %#v, want one stale worktree named feature", stale)
	}
}
