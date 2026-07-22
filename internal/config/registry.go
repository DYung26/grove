package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// DepsCloneModeNone marks a worktree whose project type has no
// dependency dirs to clone at all (e.g. pnpm, Go) — distinct from ""
// (unresolved/unknown), which means dependency dirs are expected but
// haven't been successfully cloned yet. Without this distinction,
// `grove repair` on a project with zero dependency dirs would write back
// the same "" it started with every time, making `grove status` list it
// as a repair candidate forever with nothing repair could ever fix.
const DepsCloneModeNone = "none"

type Worktree struct {
	Name      string    `json:"name"`
	Path      string    `json:"path"`
	Branch    string    `json:"branch"`
	CreatedAt time.Time `json:"created_at"`
	// DepsCloneMode records how this worktree's dependency dirs were most
	// recently placed — "reflink", "hardlink", "copy", DepsCloneModeNone if
	// the project type has no dependency dirs to manage, or "" for unknown
	// (e.g. adopted from a pre-Grove layout, or created before this field
	// existed). `grove repair` treats anything other than "reflink" or
	// DepsCloneModeNone as a candidate to retrofit, since only reflink is
	// real CoW; hardlink and copy are both already-applied fallbacks worth
	// upgrading if the destination can do better now (e.g. after `grove
	// pool init`).
	// Stored as a string rather than fs.CloneMode's int to keep this
	// package independent of internal/fs and keep the JSON human-readable.
	DepsCloneMode string `json:"deps_clone_mode,omitempty"`
}

type Registry struct {
	Worktrees []Worktree `json:"worktrees"`
}

func Load(repoRoot string) (*Registry, error) {
	data, err := os.ReadFile(registryPath(repoRoot))
	if os.IsNotExist(err) {
		return &Registry{}, nil
	}
	if err != nil {
		return nil, err
	}

	var reg Registry
	if err := json.Unmarshal(data, &reg); err != nil {
		return nil, err
	}
	return &reg, nil
}

func (r *Registry) Save(repoRoot string) error {
	path := registryPath(repoRoot)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func (r *Registry) Add(wt Worktree) {
	r.Worktrees = append(r.Worktrees, wt)
}

// Update replaces the tracked worktree with the same Name as wt.
// Reports whether an existing entry was found and replaced. wt.Name is
// used as the lookup key, so this can only update a record in place —
// it cannot change a worktree's Name itself, since the entry to replace
// is found by matching that same field. Use Rename for that.
func (r *Registry) Update(wt Worktree) bool {
	for i, existing := range r.Worktrees {
		if existing.Name == wt.Name {
			r.Worktrees[i] = wt
			return true
		}
	}
	return false
}

// Rename replaces the entry currently tracked under oldName with wt,
// which may itself carry a different Name. Unlike Update, whose lookup
// key is wt.Name and so can never match an entry it's meant to rename
// away from, Rename keys the lookup on oldName explicitly. Reports
// whether an existing entry named oldName was found and replaced.
func (r *Registry) Rename(oldName string, wt Worktree) bool {
	for i, existing := range r.Worktrees {
		if existing.Name == oldName {
			r.Worktrees[i] = wt
			return true
		}
	}
	return false
}

// Find returns the tracked worktree with the given name, if any.
func (r *Registry) Find(name string) (Worktree, bool) {
	for _, wt := range r.Worktrees {
		if wt.Name == name {
			return wt, true
		}
	}
	return Worktree{}, false
}

// Remove drops the tracked worktree with the given name, if present.
// Reports whether anything was actually removed.
func (r *Registry) Remove(name string) bool {
	for i, wt := range r.Worktrees {
		if wt.Name == name {
			r.Worktrees = append(r.Worktrees[:i], r.Worktrees[i+1:]...)
			return true
		}
	}
	return false
}

func registryPath(repoRoot string) string {
	return filepath.Join(repoRoot, ".grove", "registry.json")
}
