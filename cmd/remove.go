package cmd

import (
	"fmt"

	"github.com/dyung/grove/internal/config"
	"github.com/dyung/grove/internal/git"
	"github.com/dyung/grove/internal/output"
	"github.com/spf13/cobra"
)

var removeForce bool

var removeCmd = &cobra.Command{
	Use:     "remove <name>",
	Aliases: []string{"rm"},
	Short:   "Remove a tracked worktree",
	Args:    cobra.ExactArgs(1),
	RunE:    runRemove,
}

func init() {
	removeCmd.Flags().BoolVar(&removeForce, "force", false, "remove even if the worktree has uncommitted changes")
	rootCmd.AddCommand(removeCmd)
}

func runRemove(cmd *cobra.Command, args []string) error {
	name := args[0]

	repoRoot, err := git.MainRepoRoot()
	if err != nil {
		return err
	}

	reg, err := config.Load(repoRoot)
	if err != nil {
		return err
	}

	wt, found := reg.Find(name)
	if !found {
		return fmt.Errorf("no tracked worktree named %q (see `grove list`)", name)
	}

	if err := git.WorktreeRemove(wt.Path, removeForce); err != nil {
		return err
	}

	reg.Remove(name)
	if err := reg.Save(repoRoot); err != nil {
		return err
	}

	fmt.Printf("%s Removed worktree %s\n", output.Success("✓"), output.Name(name))
	return nil
}
