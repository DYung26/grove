package cmd

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/dyung/grove/internal/config"
	"github.com/dyung/grove/internal/git"
	"github.com/dyung/grove/internal/output"
	"github.com/spf13/cobra"
)

var renameCmd = &cobra.Command{
	Use:   "rename <old-name> <new-name>",
	Short: "Rename a tracked worktree, moving its directory and updating the registry",
	Args:  exactArgs(2),
	RunE:  runRename,
}

func init() {
	rootCmd.AddCommand(renameCmd)
}

// runRename moves a tracked worktree's directory to match a new name and
// updates the registry to match — Name is Grove's stable identifier
// (see resolveWorktreeName in create.go), so renaming it is a real,
// visible operation rather than just relabeling: the directory itself
// moves too, via `git worktree move`, so branch and any other git state
// stay correctly linked at the new path.
func runRename(cmd *cobra.Command, args []string) error {
	oldName, newName := args[0], args[1]
	if oldName == newName {
		return fmt.Errorf("new name is the same as the current name (%q)", oldName)
	}
	if strings.Contains(newName, "/") {
		return fmt.Errorf("new name %q can't contain %q — a worktree Name must be a single path segment (it's also used as the directory name and inside pool subvolume paths)", newName, "/")
	}

	repoRoot, err := git.MainRepoRoot()
	if err != nil {
		return err
	}

	reg, err := config.Load(repoRoot)
	if err != nil {
		return err
	}

	wt, found := reg.Find(oldName)
	if !found {
		return fmt.Errorf("no tracked worktree named %q (see `grove list`)", oldName)
	}
	if existing, found := reg.Find(newName); found {
		return fmt.Errorf("worktree %q already exists at %s", newName, existing.Path)
	}

	newPath := filepath.Join(filepath.Dir(wt.Path), newName)
	// newPath can already equal wt.Path when only the registry Name
	// changes but the directory it points at happens to share that
	// basename (e.g. a legacy Name like "feat/drive-picker" being
	// normalized to "feat-drive-picker" while Path already ends in
	// "feat-drive-picker"). Calling git worktree move with identical
	// old/new paths isn't a no-op: git still tries to move the resolved
	// target into what it sees as an existing destination directory,
	// nesting it inside itself and failing. Skipping the move in that case
	// is correct — there's nothing on disk to relocate, only the registry.
	if newPath != wt.Path {
		if err := git.WorktreeMove(wt.Path, newPath); err != nil {
			return err
		}
	}

	if !reg.Rename(oldName, config.Worktree{
		Name:          newName,
		Path:          newPath,
		Branch:        wt.Branch,
		CreatedAt:     wt.CreatedAt,
		DepsCloneMode: wt.DepsCloneMode,
	}) {
		// Shouldn't happen: reg.Find(oldName) above already confirmed this
		// entry exists. Surfacing it explicitly rather than silently
		// continuing, since the directory has already been moved on disk at
		// this point and a silent no-op here is exactly the bug this guards
		// against.
		return fmt.Errorf("internal error: worktree %q vanished from the registry mid-rename (directory was already moved to %s; registry not updated)", oldName, newPath)
	}
	if err := reg.Save(repoRoot); err != nil {
		return err
	}

	fmt.Printf("%s Renamed worktree %s to %s (now at %s)\n", output.Success("✓"), output.Name(oldName), output.Name(newName), output.Path(newPath))
	return nil
}
