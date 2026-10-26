package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func initTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-b", "main", dir},
		{"-C", dir, "config", "user.name", "Grove Tests"},
		{"-C", dir, "config", "user.email", "grove-tests@example.invalid"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("initial\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"-C", dir, "add", "tracked.txt"}, {"-C", dir, "commit", "-m", "initial"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return dir
}

func TestInspectWorktreeStatus(t *testing.T) {
	dir := initTestRepo(t)
	got, err := InspectWorktreeStatus(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != (WorktreeStatus{}) {
		t.Fatalf("expected clean status, got %+v", got)
	}

	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err = InspectWorktreeStatus(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Unstaged || !got.Untracked || got.Staged {
		t.Fatalf("expected unstaged and untracked only, got %+v", got)
	}

	if out, err := exec.Command("git", "-C", dir, "add", "tracked.txt").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v: %s", err, out)
	}
	got, err = InspectWorktreeStatus(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Staged || !got.Untracked {
		t.Fatalf("expected staged and untracked, got %+v", got)
	}
}

func TestAheadBehindRequiresUpstream(t *testing.T) {
	dir := initTestRepo(t)
	ahead, behind, hasUpstream, err := AheadBehind(dir)
	if err != nil {
		t.Fatal(err)
	}
	if ahead != 0 || behind != 0 || hasUpstream {
		t.Fatalf("expected unknown upstream, got ahead=%d behind=%d hasUpstream=%t", ahead, behind, hasUpstream)
	}
}
