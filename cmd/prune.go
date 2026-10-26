package cmd

import (
	"fmt"
	"os"

	"github.com/dyung/grove/internal/config"
	"github.com/dyung/grove/internal/git"
	"github.com/spf13/cobra"
)

var pruneDryRun bool

var pruneCmd = &cobra.Command{
	Use:   "prune",
	Short: "Reconcile stale worktree entries in the Grove registry with Git",
	Long:  "Find registry entries whose worktree directory has already disappeared, remove those stale entries, and clean up Git worktree administrative records left behind. It never deletes a worktree directory. Use --dry-run to preview the reconciliation without changing anything.",
	Args:  exactArgs(0),
	RunE:  runPrune,
}

func init() {
	pruneCmd.Flags().BoolVar(&pruneDryRun, "dry-run", false, "preview stale-entry reconciliation without changing anything")
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
		fmt.Println("Nothing stale to prune.")
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

		// The worktree directory is already gone, so only stale Git
		// administrative metadata and the Grove registry entry are removed.
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

// staleWorktrees returns registry entries whose worktree directory is already
// missing. A missing branch or Git admin record alone is not permission to
// destroy a directory that may still contain user work; `grove clean` handles
// extant and orphaned worktrees through an explicit user-directed flow.
func staleWorktrees(reg *config.Registry) ([]config.Worktree, error) {
	var stale []config.Worktree
	for _, wt := range reg.Worktrees {
		// Pruning reconciles metadata; it must not infer that a live directory
		// is disposable merely because its branch or Git admin record is gone.
		// User-directed deletion of extant/orphaned worktrees belongs to clean.
		if _, err := os.Stat(wt.Path); err == nil {
			continue
		} else if !os.IsNotExist(err) {
			return nil, fmt.Errorf("inspect worktree path %q: %w", wt.Path, err)
		}
		stale = append(stale, wt)
	}
	return stale, nil
}

// staleReason explains why a missing worktree directory is stale, distinguishing
// a missing local branch from a missing checkout whose branch still exists.
func staleReason(wt config.Worktree) (string, error) {
	exists, err := git.BranchExists(wt.Branch)
	if err != nil {
		return "", err
	}
	if !exists {
		return fmt.Sprintf("branch %q no longer exists", wt.Branch), nil
	}
	return "worktree directory is gone even though the branch still exists", nil
}

// removeStaleWorktree clears Git's administrative records for a worktree whose
// directory is already gone. It never removes a live checkout or its files.
func removeStaleWorktree(wt config.Worktree) error {
	// The directory is already gone, so there is no checkout for
	// WorktreeRemove to validate. Prune Git's stale administrative records
	// and let runPrune remove this entry from Grove's registry.
	return git.WorktreePrune()
}
