package cmd

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/dyung/grove/internal/config"
	"github.com/dyung/grove/internal/fs"
	"github.com/dyung/grove/internal/git"
	"github.com/dyung/grove/internal/output"
	"github.com/spf13/cobra"
)

var openPrint bool

var openCmd = &cobra.Command{
	Use:   "open <name-or-branch>",
	Short: "Open any worktree by its Grove name or git branch (including the main repo)",
	Args:  exactArgs(1),
	RunE:  runOpen,
}

func init() {
	openCmd.Flags().BoolVar(&openPrint, "print", false, "print the worktree path instead of spawning a subshell")
	rootCmd.AddCommand(openCmd)
}

func runOpen(cmd *cobra.Command, args []string) error {
	path, err := resolveOpenPath(args[0])
	if err != nil {
		return err
	}

	if openPrint {
		fmt.Println(path)
		return nil
	}

	return spawnSubshell(path)
}

// resolveOpenPath finds the worktree to open, trying Grove's own
// registry Name first (the stable identifier, e.g. from `grove create
// --name` or `grove rename`) and falling back to a live git branch
// match — the same list `git worktree list` shows — so `grove open
// main` and any worktree Grove doesn't track still work. Name is tried
// first because it's the more specific, user-chosen identifier; a
// branch happening to share that same string is the less likely case
// once Name and Branch can diverge (see resolveWorktreeName in
// create.go).
func resolveOpenPath(nameOrBranch string) (string, error) {
	repoRoot, err := git.MainRepoRoot()
	if err != nil {
		return "", err
	}

	reg, err := config.Load(repoRoot)
	if err != nil {
		return "", err
	}

	if wt, found := reg.Find(nameOrBranch); found {
		if err := requireHealthyWorktree(wt.Path, nameOrBranch); err != nil {
			return "", err
		}
		return wt.Path, nil
	}

	worktrees, err := git.ListWorktrees()
	if err != nil {
		return "", err
	}

	for _, wt := range worktrees {
		if wt.Branch != nameOrBranch {
			continue
		}
		if status := fs.CheckHealth(wt.Path); status != fs.Healthy {
			return "", fmt.Errorf("branch %q is checked out at %s, but that directory is %s", nameOrBranch, wt.Path, status)
		}
		return wt.Path, nil
	}
	return "", fmt.Errorf("no worktree tracked or checked out as %q (see `grove list` or `git worktree list`)", nameOrBranch)
}

// requireHealthyWorktree checks a registry-tracked worktree's directory
// with a real health probe rather than a plain os.Stat, so a path that
// exists but is corrupted (EIO from a broken btrfs subvolume, a mount
// that flipped read-only) gets a diagnosis naming what's actually wrong
// instead of either a raw stat error or, worse, being treated as fine
// because the stat itself succeeded. Shared by every command that looks
// a worktree up by registry entry before acting on its directory.
func requireHealthyWorktree(path, name string) error {
	status := fs.CheckHealth(path)
	if status == fs.Healthy {
		return nil
	}
	return fmt.Errorf("worktree %q is tracked but its directory is %s", name, status)
}

// spawnSubshell drops the user into an interactive shell rooted at path.
// A child process can't change its parent shell's working directory
// directly, so entering a worktree means either printing its path (for
// `cd $(grove open <branch> --print)`) or nesting a shell there instead.
// $SHELL is honored so the user's own shell and config apply; it falls
// back to /bin/sh if unset.
func spawnSubshell(path string) error {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}

	fmt.Println(output.Dim(fmt.Sprintf("Entering %s (subshell; type `exit` to return)", output.Path(path))))

	sub := exec.Command(shell)
	sub.Dir = path
	sub.Stdin = os.Stdin
	sub.Stdout = os.Stdout
	sub.Stderr = os.Stderr
	return sub.Run()
}
