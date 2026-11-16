package cmd

import (
	"path/filepath"
	"testing"

	"github.com/dyung/grove/internal/fs"
)

func TestRepoHasNativeReflink(t *testing.T) {
	repoRoot := filepath.Join(t.TempDir(), "project")
	var detectedPath string

	if !repoHasNativeReflink(repoRoot, func(path string) fs.CloneSupport {
		detectedPath = path
		return fs.CloneSupported
	}) {
		t.Fatal("repo with native reflink support should skip pool initialization")
	}
	if detectedPath != filepath.Dir(repoRoot) {
		t.Fatalf("detected filesystem at %q, want worktree parent %q", detectedPath, filepath.Dir(repoRoot))
	}

	if repoHasNativeReflink(repoRoot, func(string) fs.CloneSupport {
		return fs.CloneUnsupported
	}) {
		t.Fatal("repo without native reflink support should still be eligible for pool initialization")
	}
	if repoHasNativeReflink(repoRoot, func(string) fs.CloneSupport {
		return fs.CloneUnknown
	}) {
		t.Fatal("unknown filesystem support must not be treated as native reflink support")
	}
}
