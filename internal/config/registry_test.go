package config

import (
	"testing"
	"time"
)

func TestRegistrySaveThenLoadRoundTrips(t *testing.T) {
	repoRoot := t.TempDir()

	reg := &Registry{}
	reg.Add(Worktree{
		Name:          "feature-x",
		Path:          "/repo/.wt/feature-x",
		Branch:        "feature-x",
		CreatedAt:     time.Now().Truncate(time.Second),
		DepsCloneMode: "reflink",
	})

	if err := reg.Save(repoRoot); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := Load(repoRoot)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if len(loaded.Worktrees) != 1 {
		t.Fatalf("got %d worktrees, want 1", len(loaded.Worktrees))
	}

	want := reg.Worktrees[0]
	got := loaded.Worktrees[0]
	if got.Name != want.Name || got.Path != want.Path || got.Branch != want.Branch || got.DepsCloneMode != want.DepsCloneMode {
		t.Errorf("round-tripped worktree = %+v, want %+v", got, want)
	}
	if !got.CreatedAt.Equal(want.CreatedAt) {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, want.CreatedAt)
	}
}

func TestLoadWithNoRegistryFileReturnsEmptyRegistry(t *testing.T) {
	repoRoot := t.TempDir()

	reg, err := Load(repoRoot)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(reg.Worktrees) != 0 {
		t.Errorf("got %d worktrees, want 0", len(reg.Worktrees))
	}
}

func TestRegistryFindReturnsTrackedWorktreeByName(t *testing.T) {
	reg := &Registry{}
	reg.Add(Worktree{Name: "a", Path: "/tmp/a", Branch: "a"})

	got, found := reg.Find("a")
	if !found {
		t.Fatalf("Find(%q): expected found = true", "a")
	}
	if got.Path != "/tmp/a" {
		t.Errorf("Find(%q).Path = %q, want %q", "a", got.Path, "/tmp/a")
	}

	if _, found := reg.Find("missing"); found {
		t.Errorf("Find(%q): expected found = false", "missing")
	}
}

func TestRegistryUpdateReplacesExistingEntryByName(t *testing.T) {
	reg := &Registry{}
	reg.Add(Worktree{Name: "a", Path: "/tmp/a", Branch: "a"})

	replaced := reg.Update(Worktree{Name: "a", Path: "/tmp/a-new", Branch: "a", DepsCloneMode: "reflink"})
	if !replaced {
		t.Fatalf("Update: expected an existing entry to be replaced")
	}

	got, _ := reg.Find("a")
	if got.Path != "/tmp/a-new" || got.DepsCloneMode != "reflink" {
		t.Errorf("Update did not apply, got %+v", got)
	}

	if reg.Update(Worktree{Name: "missing", Path: "/tmp/x"}) {
		t.Errorf("Update: expected false when no entry matches the name")
	}
}

func TestRegistryRemoveDropsEntryByName(t *testing.T) {
	reg := &Registry{}
	reg.Add(Worktree{Name: "a", Path: "/tmp/a", Branch: "a"})
	reg.Add(Worktree{Name: "b", Path: "/tmp/b", Branch: "b"})

	if !reg.Remove("a") {
		t.Fatalf("Remove(%q): expected true", "a")
	}
	if _, found := reg.Find("a"); found {
		t.Errorf("expected %q to be gone after Remove", "a")
	}
	if _, found := reg.Find("b"); !found {
		t.Errorf("expected %q to remain untouched", "b")
	}

	if reg.Remove("a") {
		t.Errorf("Remove(%q): expected false for an already-removed name", "a")
	}
}
