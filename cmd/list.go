package cmd

import (
	"fmt"

	"github.com/dyung/grove/internal/config"
	"github.com/dyung/grove/internal/git"
	"github.com/dyung/grove/internal/output"
	"github.com/spf13/cobra"
)

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List worktrees tracked by Grove",
	RunE:  runList,
}

func init() {
	rootCmd.AddCommand(listCmd)
}

func runList(cmd *cobra.Command, args []string) error {
	repoRoot, err := git.MainRepoRoot()
	if err != nil {
		return err
	}

	reg, err := config.Load(repoRoot)
	if err != nil {
		return err
	}

	if len(reg.Worktrees) == 0 {
		fmt.Println(output.Dim("No worktrees tracked yet."))
		return nil
	}

	for _, wt := range reg.Worktrees {
		fmt.Println(describeListEntry(wt))
	}
	return nil
}

// describeListEntry formats one registry entry, always showing all
// three identifiers a worktree has — name, branch, and path — rather
// than hiding branch when it happens to equal name. Name and Branch can
// look identical as strings while still being independent identifiers
// (Name is Grove's own, only ever changed via `grove rename`; Branch is
// git's, and can change independently via a plain `git checkout -b`
// inside the worktree) — collapsing the display whenever they match
// hides that they're two different things that merely coincide right
// now, which is exactly the case a user needs `grove rename` for and
// can't see the need for if the branch line is suppressed.
func describeListEntry(wt config.Worktree) string {
	return fmt.Sprintf("%s\t(branch %s)\t%s", output.Name(wt.Name), wt.Branch, output.Path(wt.Path))
}
