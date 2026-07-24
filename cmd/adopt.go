package cmd

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/dyung/grove/internal/config"
	"github.com/dyung/grove/internal/git"
	"github.com/dyung/grove/internal/output"
	"github.com/spf13/cobra"
)

var adoptCmd = &cobra.Command{
	Use:   "adopt <path>",
	Short: "Register an existing git worktree that Grove didn't create",
	Args:  exactArgs(1),
	RunE:  runAdopt,
}

var adoptName string

func init() {
	adoptCmd.Flags().StringVar(&adoptName, "name", "", "worktree name to register under (default: the directory's own name)")
	rootCmd.AddCommand(adoptCmd)
}

// runAdopt registers a worktree Grove didn't create itself: either an old
// manual sibling checkout from before adopting Grove, or a plain
// directory sitting inside a pool-backed .wt container from before the
// pool existed. It only touches the registry — no git or filesystem
// mutation, no dependency-dir cloning or CoW retrofit. A worktree with
// out-of-date (non-reflinked) dependency dirs stays that way until
// `grove repair` is run against it separately.
func runAdopt(cmd *cobra.Command, args []string) error {
	path := args[0]

	repoRoot, err := git.MainRepoRoot()
	if err != nil {
		return err
	}

	wtInfo, found, err := git.FindWorktree(path)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%q is not a git worktree of this repo (see `git worktree list`)", path)
	}
	if wtInfo.Branch == "" {
		return fmt.Errorf("%q is a bare or detached worktree (no branch); grove adopt requires a branch-based worktree", path)
	}

	reg, err := config.Load(repoRoot)
	if err != nil {
		return err
	}

	name := adoptName
	if name == "" {
		// The directory's own basename, not the branch: an adopted worktree
		// already has a real path on disk (e.g. assessly.wt/on-hold), and
		// that's the stable, user-chosen identifier — the branch checked out
		// there can be renamed or swapped later without the worktree itself
		// moving.
		name = filepath.Base(wtInfo.Path)
	}

	if existing, found := reg.Find(name); found {
		return fmt.Errorf("worktree %q is already tracked at %s; pass --name to register this one under a different name", name, existing.Path)
	}

	reg.Add(config.Worktree{
		Name:   name,
		Path:   wtInfo.Path,
		Branch: wtInfo.Branch,
		// CreatedAt records when Grove started tracking it, not when the
		// worktree itself was actually created — that history predates
		// Grove and isn't recoverable from git worktree metadata.
		CreatedAt: time.Now(),
	})

	if err := reg.Save(repoRoot); err != nil {
		return err
	}

	fmt.Println(describeAdopted(name, wtInfo.Branch, wtInfo.Path))
	fmt.Println(output.Dim(fmt.Sprintf("Its dependency dirs haven't been touched — run `grove repair %s` to reflink them.", name)))
	return nil
}

// describeAdopted reports the worktree's name, branch, and path, always
// all three — see describeListEntry in list.go for why Name and Branch
// are shown even when they're the same string right now.
func describeAdopted(name, branch, path string) string {
	return fmt.Sprintf("%s Adopted worktree %s (branch %s) at %s", output.Success("✓"), output.Name(name), branch, output.Path(path))
}
