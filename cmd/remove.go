package cmd

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
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

	resolvedPath := resolveWorktreeRemovePath(wt.Path)
	if err := git.WorktreeRepair(resolvedPath); err != nil {
		return fmt.Errorf("repair worktree metadata for %q before removal: %w", name, err)
	}
	if err := git.WorktreeRemove(resolvedPath, removeForce); err != nil {
		return err
	}

	reg.Remove(name)
	if err := reg.Save(repoRoot); err != nil {
		return err
	}

	fmt.Printf("%s Removed worktree %s\n", output.Success("✓"), output.Name(name))
	return removeBranchIfWanted(wt.Branch)
}

// resolveWorktreeRemovePath resolves path (the registry's recorded
// worktree path, always the <repo>.wt/<name> location) through any
// symlink before handing it to git.WorktreeRepair and git.WorktreeRemove.
// A pool-resident worktree lives at that path only as a symlink into the
// Grove pool — EnsureWorktreeVolume swaps the real directory there for
// one once migration completes (see internal/pool/repo.go) — and `git
// worktree remove` refuses to operate through a symlink at all ("Not a
// directory"). Resolving the symlink alone isn't sufficient on its own,
// though: git's own .git/worktrees/<name>/gitdir record still points at
// the original symlink path from when `git worktree add` first ran, so
// WorktreeRepair also needs the resolved path to bring that record back
// in sync before removal — otherwise `git worktree remove` reads the
// stale gitdir internally and fails or leaves the worktree half torn
// down even when called with a correct, resolved path argument. Mirrors
// isPoolResident's own EvalSymlinks-based resolution elsewhere in this
// package. If path isn't a symlink (or doesn't exist), EvalSymlinks
// returns it unchanged.
func resolveWorktreeRemovePath(path string) string {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return path
	}
	return resolved
}

// removeBranchIfWanted asks whether to delete branch now that its
// worktree is gone, mirroring `git worktree remove`'s own scope: it only
// ever removes the worktree checkout and git's internal record for it,
// never touches the branch ref itself, so a leftover branch after `grove
// remove` is expected git behavior, not a bug — this is Grove choosing
// to offer the follow-up rather than leaving the user to remember
// `git branch -d` themselves. Skipped entirely under --force, matching
// removeForce's documented scope (skips confirmation prompts), rather
// than force-deleting a branch the user never explicitly asked to lose.
func removeBranchIfWanted(branch string) error {
	if removeForce || branch == "" {
		return nil
	}

	exists, err := git.BranchExists(branch)
	if err != nil {
		return err
	}
	if !exists {
		// Already gone (e.g. deleted manually, or never existed as a real
		// local branch — an adopted worktree's Branch field can be empty or
		// stale). Nothing to offer.
		return nil
	}

	confirmed, err := confirmBranchDelete(branch)
	if err != nil {
		return err
	}
	if !confirmed {
		return nil
	}

	ok, err := git.BranchDeleteSafe(branch)
	if err != nil {
		return fmt.Errorf("delete branch %q: %w", branch, err)
	}
	if ok {
		fmt.Printf("%s Deleted branch %s\n", output.Success("✓"), output.Name(branch))
		return nil
	}

	return deleteUnmergedBranch(branch)
}

// deleteUnmergedBranch handles the case git branch -d itself refused:
// branch has commits not merged anywhere else, so deleting it would
// discard them permanently. This is confirmed as a second, more
// explicit prompt rather than silently escalating to -D, since it's a
// meaningfully more destructive action than the merged-branch case the
// user already agreed to.
func deleteUnmergedBranch(branch string) error {
	fmt.Printf("%s Branch %q has commits not merged anywhere else — deleting it would discard them permanently.\n", output.Warn("⚠"), branch)
	fmt.Print("Force-delete it anyway? [y/N] ")

	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil {
		return err
	}

	answer := strings.ToLower(strings.TrimSpace(line))
	if answer != "y" && answer != "yes" {
		fmt.Printf("Branch %s left in place.\n", output.Name(branch))
		return nil
	}

	if err := git.BranchDeleteForce(branch); err != nil {
		return fmt.Errorf("force-delete branch %q: %w", branch, err)
	}
	fmt.Printf("%s Force-deleted branch %s\n", output.Success("✓"), output.Name(branch))
	return nil
}

// confirmBranchDelete asks whether to delete branch now that its
// worktree is gone, the same [y/N] pattern the rest of this file uses
// for consequential-but-recoverable actions.
func confirmBranchDelete(branch string) (bool, error) {
	fmt.Printf("Also delete branch %s? [y/N] ", output.Name(branch))

	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil {
		return false, err
	}

	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes", nil
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
