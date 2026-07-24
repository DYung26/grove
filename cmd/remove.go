package cmd

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/dyung/grove/internal/config"
	"github.com/dyung/grove/internal/fs"
	"github.com/dyung/grove/internal/git"
	"github.com/dyung/grove/internal/output"
	"github.com/dyung/grove/internal/pool"
	"github.com/spf13/cobra"
)

var removeForce bool

var removeCmd = &cobra.Command{
	Use:     "remove <name>",
	Aliases: []string{"rm"},
	Short:   "Remove a tracked worktree",
	Args:    exactArgs(1),
	RunE:    runRemove,
}

func init() {
	removeCmd.Flags().BoolVar(&removeForce, "force", false, "remove even if the worktree has uncommitted changes; also skips confirmation before deleting a corrupted pool subvolume")
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

	if status := fs.CheckHealth(wt.Path); status != fs.Healthy {
		return removeUnhealthyWorktree(repoRoot, reg, wt, status)
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

// removeUnhealthyWorktree handles a tracked worktree whose directory
// fails a health check — git.WorktreeRemove would fail here anyway,
// since `git worktree remove` needs to validate the directory (find
// .git, check for a dirty working tree) and can't do that against a
// path where every syscall returns EIO. If the worktree is pool-resident
// (a symlink into the Grove pool), the actual, narrow fix is deleting
// just that one subvolume, not the whole pool — offered here after
// confirmation, since it permanently discards the worktree's contents.
// A plain (non-pool) worktree in this state has no narrower fix Grove
// can offer; the underlying filesystem itself needs manual attention.
func removeUnhealthyWorktree(repoRoot string, reg *config.Registry, wt config.Worktree, status fs.HealthStatus) error {
	if status == fs.Missing {
		return removeMissingWorktree(repoRoot, reg, wt)
	}

	paths, err := pool.DefaultPaths()
	if err != nil {
		return err
	}

	if !isPoolResident(wt.Path, paths.MountPoint) {
		return fmt.Errorf("worktree %q is tracked but its directory is %s, and it isn't pool-resident, so Grove has no automated fix — investigate the underlying filesystem manually", wt.Name, status)
	}

	mounted, err := pool.IsMounted(paths.MountPoint)
	if err != nil {
		return err
	}
	if !mounted {
		return fmt.Errorf("worktree %q resolves into the Grove pool, but the pool isn't mounted right now — run %s, then re-run `grove remove %s`", wt.Name, output.Command("grove pool init"), wt.Name)
	}

	if readOnly, err := pool.IsReadOnly(paths.MountPoint); err != nil {
		return err
	} else if readOnly {
		return fmt.Errorf("the Grove pool itself is read-only, so its subvolumes can't be deleted yet — run %s first, then re-run `grove remove %s`", output.Command("grove pool repair"), wt.Name)
	}

	if !removeForce {
		confirmed, err := confirmSubvolumeDelete(wt.Name, status)
		if err != nil {
			return err
		}
		if !confirmed {
			return fmt.Errorf("aborted: worktree %q left as-is", wt.Name)
		}
	}

	if err := pool.DeleteSubvolume(wt.Path, paths.MountPoint); err != nil {
		return fmt.Errorf("delete corrupted subvolume for %q: %w", wt.Name, err)
	}

	// The worktree directory is gone from disk now (subvolume deleted, not
	// just unreadable), so `git worktree remove` would fail the same way a
	// manually `rm -rf`'d worktree does. `git worktree prune` is the correct
	// git-side cleanup for a worktree whose directory no longer exists.
	if err := git.WorktreePrune(); err != nil {
		return fmt.Errorf("deleted the corrupted subvolume, but `git worktree prune` failed: %w (registry entry left in place; re-run `grove remove %s` or clean up manually)", err, wt.Name)
	}

	reg.Remove(wt.Name)
	if err := reg.Save(repoRoot); err != nil {
		return err
	}

	fmt.Printf("%s Deleted the corrupted subvolume and removed worktree %s\n", output.Success("✓"), output.Name(wt.Name))
	return nil
}

// removeMissingWorktree handles a tracked worktree whose directory is
// gone from disk entirely (fs.Missing) rather than merely unreadable or
// read-only — the case a pool.Recreate leaves behind, since recreating
// the pool deletes the whole image without touching Grove's registry.
// There's no subvolume left to delete (DeleteSubvolume would just fail
// against a nonexistent path), so this skips straight to the git-side
// and registry cleanup: git worktree prune for the stale
// .git/worktrees/ entry, same as removeUnhealthyWorktree does after an
// actual subvolume delete, then dropping the registry record. No
// confirmation prompt, unlike the corrupted-subvolume path — there's no
// remaining data this could destroy that isn't already gone.
func removeMissingWorktree(repoRoot string, reg *config.Registry, wt config.Worktree) error {
	if err := git.WorktreePrune(); err != nil {
		return fmt.Errorf("worktree %q's directory is gone, but `git worktree prune` failed: %w (registry entry left in place; re-run `grove remove %s` or clean up manually)", wt.Name, err, wt.Name)
	}

	reg.Remove(wt.Name)
	if err := reg.Save(repoRoot); err != nil {
		return err
	}

	fmt.Printf("%s Worktree %s's directory was already gone (e.g. from a pool rebuild) — pruned git's stale entry and untracked it.\n", output.Success("✓"), output.Name(wt.Name))
	return nil
}

// confirmSubvolumeDelete asks before permanently discarding a corrupted
// worktree's contents, the same [y/N] pattern confirmPoolRecreate
// (pool.go) uses for its larger-scope equivalent.
func confirmSubvolumeDelete(name string, status fs.HealthStatus) (bool, error) {
	fmt.Printf("%s Worktree %q is %s and pool-resident. Grove can delete just its subvolume, permanently discarding its contents (dependency dirs, any uncommitted work in it).\n",
		output.Warn("⚠"), name, status)
	fmt.Println("Its branch and any committed history are safe in git regardless. Re-run `grove create` or `grove repair` afterward to get a fresh worktree back.")
	fmt.Print("Delete the corrupted subvolume? [y/N] ")

	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil {
		return false, err
	}

	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes", nil
}
