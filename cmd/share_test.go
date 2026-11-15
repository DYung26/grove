package cmd

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReconcileSharedPathKeepsExistingCopyByDefault(t *testing.T) {
	root, worktree := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("canonical"), 0o600); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(worktree, ".env")
	if err := os.WriteFile(destination, []byte("worktree"), 0o600); err != nil {
		t.Fatal(err)
	}
	backup, linked, err := reconcileSharedPath(bufio.NewReader(strings.NewReader("\n")), root, worktree, ".env")
	if err != nil {
		t.Fatal(err)
	}
	if linked || backup != "" {
		t.Fatalf("reconcile result = (%q, %v), want no link", backup, linked)
	}
	got, err := os.ReadFile(destination)
	if err != nil || string(got) != "worktree" {
		t.Fatalf("existing copy changed: %q, %v", got, err)
	}
}

func TestReconcileSharedPathLinksOnlyAfterExplicitConfirmation(t *testing.T) {
	root, worktree := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("canonical"), 0o600); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(worktree, ".env")
	if err := os.WriteFile(destination, []byte("worktree"), 0o600); err != nil {
		t.Fatal(err)
	}
	backup, linked, err := reconcileSharedPath(bufio.NewReader(strings.NewReader("l\ny\n")), root, worktree, ".env")
	if err != nil {
		t.Fatal(err)
	}
	if !linked || backup != destination+".grove-bak" {
		t.Fatalf("reconcile result = (%q, %v), want preserved backup and link", backup, linked)
	}
	if info, err := os.Lstat(destination); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("destination is not a symlink: info=%v err=%v", info, err)
	}
	got, err := os.ReadFile(backup)
	if err != nil || string(got) != "worktree" {
		t.Fatalf("backup contents = %q, error = %v", got, err)
	}
}
