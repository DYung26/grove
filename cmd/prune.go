package cmd

import (
	"fmt"

	"github.com/dyung/grove/internal/config"
	"github.com/dyung/grove/internal/git"
	"github.com/spf13/cobra"
)

var pruneDryRun bool

var pruneCmd = &cobra.Command{
	Use:   "prune",
	Short: "Remove tracked worktrees whose branch is gone or whose git worktree record no longer exists",
	RunE:  runPrune,
}

func init() {
	pruneCmd.Flags().BoolVar(&pruneDryRun, "dry-run", false, "list what would be removed without removing it")
	rootCmd.AddCommand(pruneCmd)
}

func runPrune(cmd *cobra.Command, args []string) error {
	repoRoot, err := git.MainRepoRoot()
	if err != nil {
		return err
	}

	reg, err := config.Load(repoRoot)
	if err != nil {
		return err
	}

	stale, err := staleWorktrees(reg)
	if err != nil {
		return err
	}

	if len(stale) == 0 {
		fmt.Println("Nothing to prune.")
		return nil
	}

	for _, wt := range stale {
		reason, err := staleReason(wt)
		if err != nil {
			return err
		}

		if pruneDryRun {
			fmt.Printf("Would remove %s (%s)\n", wt.Name, reason)
			continue
		}

		// removeStaleWorktree picks WorktreeRemove vs WorktreePrune based
		// on whether the worktree still has a live git record to remove
		// or is already gone from `git worktree list` and only needs its
		// dangling administrative state cleared.
		if err := removeStaleWorktree(wt); err != nil {
			return fmt.Errorf("prune %s: %w", wt.Name, err)
		}
		reg.Remove(wt.Name)
		fmt.Printf("Removed %s (%s)\n", wt.Name, reason)
	}

	if pruneDryRun {
		return nil
	}
	return reg.Save(repoRoot)
}

// staleWorktrees returns the tracked worktrees that no longer belong in
// the registry, for either of two independent reasons:
//
//  1. The branch itself has been deleted (the original check).
//  2. The worktree's own git administrative record is gone from `git
//     worktree list` even though the branch still exists — the reverse
//     of case 1, left uncovered until now. This happens when something
//     removes the worktree without going through Grove, e.g. a bare
//     `git worktree remove` run by hand, or `git worktree prune` firing
//     on its own after the directory disappeared outside git's
//     knowledge. BranchExists alone can't see this: the branch is still
//     there, so case 1 never fires, and the registry entry lingers
//     forever, immune to `grove prune`, with nothing else pointing it
//     out (status.go's unhealthyCandidates only probes the filesystem
//     path itself via fs.CheckHealth, which says nothing about whether
//     git still considers that path one of its worktrees).
//
// Both reasons are reported as "stale" uniformly — pruning is the same
// registry-removal action either way — but staleReason names which one
// actually applied, so the printed message doesn't claim a branch was
// deleted when it wasn't.
func staleWorktrees(reg *config.Registry) ([]config.Worktree, error) {
	gitWorktrees, err := git.ListWorktrees()
	if err != nil {
		return nil, err
	}
	knownPaths := make(map[string]bool, len(gitWorktrees))
	for _, wt := range gitWorktrees {
		knownPaths[resolvePath(wt.Path)] = true
	}

	var stale []config.Worktree
	for _, wt := range reg.Worktrees {
		exists, err := git.BranchExists(wt.Branch)
		if err != nil {
			return nil, err
		}
		if !exists {
			stale = append(stale, wt)
			continue
		}
		if !knownPaths[resolvePath(wt.Path)] {
			stale = append(stale, wt)
		}
	}
	return stale, nil
}

// staleReason explains why wt was returned by staleWorktrees, so the
// prune output names the actual cause instead of always blaming the
// branch. Recomputes both checks rather than threading a reason value
// back out of staleWorktrees — prune only calls this per stale entry,
// so the extra git calls are proportional to what's actually being
// reported on, not a hot path.
func staleReason(wt config.Worktree) (string, error) {
	exists, err := git.BranchExists(wt.Branch)
	if err != nil {
		return "", err
	}
	if !exists {
		return fmt.Sprintf("branch %q no longer exists", wt.Branch), nil
	}
	return "its git worktree record is gone (removed outside Grove) even though the branch still exists", nil
}

// removeStaleWorktree performs the actual git-side cleanup for one stale
// entry, choosing between WorktreeRemove and WorktreePrune depending on
// which of staleWorktrees' two cases applied. WorktreeRemove requires a
// live, readable worktree directory to validate against (it shells out
// to `git worktree remove`, which fails outright otherwise) — correct
// for case 1 (branch deleted, but the worktree itself is still a normal,
// present directory). Case 2 (worktree already gone from `git worktree
// list`) has nothing live left for WorktreeRemove to act on; the only
// correct git-side step is WorktreePrune, clearing the now-dangling
// administrative record under .git/worktrees/ — the same operation
// already used elsewhere in Grove (cmd/remove.go's removeUnhealthyWorktree)
// for exactly this "directory's gone, git doesn't know yet" situation.
func removeStaleWorktree(wt config.Worktree) error {
	worktrees, err := git.ListWorktrees()
	if err != nil {
		return err
	}

	target := resolvePath(wt.Path)
	for _, gwt := range worktrees {
		if resolvePath(gwt.Path) == target {
			return git.WorktreeRemove(wt.Path, true)
		}
	}
	return git.WorktreePrune()
}
