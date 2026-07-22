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
	Short: "Remove tracked worktrees whose branch no longer exists",
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
		if pruneDryRun {
			fmt.Printf("Would remove %s (branch %q no longer exists)\n", wt.Name, wt.Branch)
			continue
		}

		if err := git.WorktreeRemove(wt.Path, true); err != nil {
			return fmt.Errorf("prune %s: %w", wt.Name, err)
		}
		reg.Remove(wt.Name)
		fmt.Printf("Removed %s (branch %q no longer exists)\n", wt.Name, wt.Branch)
	}

	if pruneDryRun {
		return nil
	}
	return reg.Save(repoRoot)
}

// staleWorktrees returns the tracked worktrees whose branch has been
// deleted since they were created.
func staleWorktrees(reg *config.Registry) ([]config.Worktree, error) {
	var stale []config.Worktree
	for _, wt := range reg.Worktrees {
		exists, err := git.BranchExists(wt.Branch)
		if err != nil {
			return nil, err
		}
		if !exists {
			stale = append(stale, wt)
		}
	}
	return stale, nil
}
