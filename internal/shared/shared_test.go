package shared

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

func TestResolveIncludesDefaultsAndConfiguredPaths(t *testing.T) {
	root := t.TempDir()
	if output, err := exec.Command("git", "-C", root, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	for _, path := range []string{".env", ".env.local", ".env.example", ".env.production", ".env.development.example", ".envrc", "apps/api/.env", "node_modules/pkg/.env", "docs/_local/notes.md", "private/config.json"} {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("data"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if output, err := exec.Command("git", "-C", root, "add", ".env.production").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v: %s", err, output)
	}
	got, err := Resolve(root, []string{"private/config.json"}, []string{".env.local"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{".env", ".envrc", "apps/api/.env", "docs/_local", "private/config.json"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Resolve() = %v, want %v", got, want)
	}
}

func TestLinkCreatesSymlinkAndIsIdempotent(t *testing.T) {
	root, worktree := t.TempDir(), t.TempDir()
	source := filepath.Join(root, ".env")
	if err := os.WriteFile(source, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Link(root, worktree, ".env", false); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(worktree, ".env")
	info, err := os.Lstat(destination)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("destination is not a symlink")
	}
	if _, err := Link(root, worktree, ".env", false); err != nil {
		t.Fatalf("idempotent Link: %v", err)
	}
	got, err := os.ReadFile(destination)
	if err != nil || string(got) != "secret" {
		t.Fatalf("linked contents = %q, error = %v", got, err)
	}
}

func TestLinkPreservesExistingDestinationUnlessReplacementRequested(t *testing.T) {
	root, worktree := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("shared"), 0o600); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(worktree, ".env")
	if err := os.WriteFile(destination, []byte("local"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Link(root, worktree, ".env", false); !errors.Is(err, ErrDestinationExists) {
		t.Fatalf("Link without replacement error = %v", err)
	}
	backup, err := Link(root, worktree, ".env", true)
	if err != nil {
		t.Fatal(err)
	}
	if backup != destination+".grove-bak" {
		t.Fatalf("backup = %q", backup)
	}
	got, err := os.ReadFile(backup)
	if err != nil || string(got) != "local" {
		t.Fatalf("backup contents = %q, error = %v", got, err)
	}
	info, err := os.Lstat(destination)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("replacement is not a symlink: info=%v err=%v", info, err)
	}
}

func TestLinkRefusesToReplaceWhenBackupExists(t *testing.T) {
	root, worktree := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("shared"), 0o600); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(worktree, ".env")
	if err := os.WriteFile(destination, []byte("local"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination+".grove-bak", []byte("old backup"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Link(root, worktree, ".env", true); err == nil {
		t.Fatal("expected existing backup to block replacement")
	}
	got, err := os.ReadFile(destination)
	if err != nil || string(got) != "local" {
		t.Fatalf("destination changed: %q, %v", got, err)
	}
}

func TestResolveRejectsPathsOutsideRepository(t *testing.T) {
	for _, path := range []string{"../outside", "docs/../outside"} {
		if _, err := Resolve(t.TempDir(), []string{path}, nil); err == nil {
			t.Fatalf("expected unsafe path %q to be rejected", path)
		}
	}
}

func TestLinkRefusesSymlinkedParentDirectory(t *testing.T) {
	root, worktree, outside := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config", "config.json"), []byte("shared"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(worktree, "config")); err != nil {
		t.Fatal(err)
	}
	if _, err := Link(root, worktree, "config/config.json", false); err == nil {
		t.Fatal("expected symlinked parent directory to be rejected")
	}
	if _, err := os.Lstat(filepath.Join(outside, "config.json")); !os.IsNotExist(err) {
		t.Fatalf("unexpected file created outside worktree: %v", err)
	}
}
