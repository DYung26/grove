package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:           "grove",
	Short:         "Grove manages Git worktrees with fast, disk-efficient environments",
	SilenceErrors: true,
	SilenceUsage:  true,
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

// exactArgs returns a cobra.PositionalArgs validator requiring exactly n
// arguments, like cobra.ExactArgs(n), but on a mismatch reports cmd.Use
// (e.g. "create <branch>", "rename <old-name> <new-name>") instead of
// Cobra's default "accepts N arg(s), received M" — that default never
// says what the missing or extra argument actually is, which matters
// most for commands whose Use string names more than one positional
// argument (rename's old-name/new-name are easy to conflate with each
// other or with the worktree's branch) but is worth the same clarity
// even for a single argument (create <branch> failing with just "accepts
// 1 arg(s), received 0" doesn't say the missing thing is a branch).
// Every command taking positional arguments uses this instead of
// cobra.ExactArgs directly, so the message is consistent across all of
// them rather than only the ones that happened to need it fixed first.
func exactArgs(n int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) != n {
			return fmt.Errorf("usage: %s", cmd.Use)
		}
		return nil
	}
}

