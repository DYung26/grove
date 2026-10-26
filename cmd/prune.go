package cmd

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/dyung/grove/internal/config"
	"github.com/dyung/grove/internal/fs"
	"github.com/dyung/grove/internal/git"
	"github.com/dyung/grove/internal/output"
	"github.com/dyung/grove/internal/pool"
	"github.com/spf13/cobra"
)

var pruneDryRun bool
var pruneAll bool
var pruneForce bool

var pruneCmd = &cobra.Command{
	Use:   "prune [name]",
	Short: "Review worktrees and remove selected worktrees or stale entries",
	Long:  "Without a name, interactively list tracked worktrees with filesystem, Git dirtiness, and upstream push status before choosing one to delete. With a name, remove that worktree. Use --stale to remove only entries whose branch or Git worktree record is already gone; --dry-run previews stale cleanup.",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runPrune,
}

func init() {
	pruneCmd.Flags().BoolVar(&pruneDryRun, "dry-run", false, "preview stale-entry cleanup without changing anything")
	pruneCmd.Flags().BoolVar(&pruneAll, "stale", false, "remove only stale registry/Git worktree entries (legacy pruning behavior)")
	pruneCmd.Flags().BoolVar(&pruneForce, "force", false, "skip confirmation and force removal despite uncommitted changes")
	rootCmd.AddCommand(pruneCmd)
}

func runPrune(cmd *cobra.Command, args []string) error {
	if pruneAll && pruneDryRun && len(args) != 0 {
		return fmt.Errorf("grove prune <name> does not accept a name with --stale or --dry-run")
	}
	if (pruneAll || pruneDryRun) && pruneForce {
		return fmt.Errorf("--force cannot be combined with --stale or --dry-run")
	}
	if !pruneAll && !pruneDryRun {
		if len(args) == 1 {
			return removeNamedWorktree(args[0], pruneForce)
		}
		if pruneForce {
			return fmt.Errorf("--force requires a worktree name: grove prune <name> --force")
		}
		return runInteractivePrune()
	}
	return runStalePrune(cmd, args)
}

func runStalePrune(cmd *cobra.Command, args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("grove prune <name> does not accept a name with --stale or --dry-run")
	}
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

// pruneCandidate is the read-only inventory displayed by grove prune.
type pruneCandidate struct {
	Worktree    config.Worktree
	Health      fs.HealthStatus
	Staged      bool
	Unstaged    bool
	Untracked   bool
	Ahead       int
	Behind      int
	HasUpstream bool
	GitRecord   bool
	Reason      string
	Safe        bool
}

// runInteractivePrune inventories registry entries, lets the user choose one,
// then delegates actual deletion to the same recovery-aware remove path as
// grove remove. Nothing is mutated while building or displaying the inventory.
func runInteractivePrune() error {
	repoRoot, err := git.MainRepoRoot()
	if err != nil {
		return err
	}
	reg, err := config.Load(repoRoot)
	if err != nil {
		return err
	}
	candidates, err := inspectPruneCandidates(reg)
	if err != nil {
		return err
	}
	if len(candidates) == 0 {
		fmt.Println("No tracked worktrees to prune.")
		return nil
	}

	fmt.Println("Tracked worktrees (safety is advisory; deleting a worktree discards its uncommitted files):")
	for i, item := range candidates {
		fmt.Printf("%2d) %-24s health=%-12s staged=%-3s unstaged=%-3s untracked=%-3s ",
			i+1, item.Worktree.Name, item.Health,
			yesNo(item.Staged), yesNo(item.Unstaged), yesNo(item.Untracked))
		switch {
		case !item.GitRecord:
			fmt.Print("push=unknown (no Git worktree record)")
		case !item.HasUpstream:
			fmt.Print("push=unknown (no upstream)")
		default:
			fmt.Printf("push=ahead %d / behind %d", item.Ahead, item.Behind)
		}
		if item.Safe {
			fmt.Print("  [clean]")
		} else {
			fmt.Printf("  [CAUTION: %s]", item.Reason)
		}
		fmt.Printf("\n    branch: %s\n    path:   %s\n", item.Worktree.Branch, item.Worktree.Path)
	}
	fmt.Print("Select a worktree number to delete (Enter to cancel): ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	choice := strings.TrimSpace(line)
	if choice == "" {
		fmt.Println("Cancelled; no worktrees changed.")
		return nil
	}
	index, err := strconv.Atoi(choice)
	if err != nil || index < 1 || index > len(candidates) {
		return fmt.Errorf("invalid selection %q; choose a number from 1 to %d", choice, len(candidates))
	}
	selected := candidates[index-1]
	fmt.Printf("Delete worktree %q (%s)? This never deletes its branch. [y/N] ", selected.Worktree.Name, selected.Worktree.Branch)
	answer, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if !strings.EqualFold(strings.TrimSpace(answer), "y") && !strings.EqualFold(strings.TrimSpace(answer), "yes") {
		fmt.Println("Cancelled; no worktrees changed.")
		return nil
	}
	return removeNamedWorktree(selected.Worktree.Name, false)
}

func inspectPruneCandidates(reg *config.Registry) ([]pruneCandidate, error) {
	gitWorktrees, err := git.ListWorktrees()
	if err != nil {
		return nil, err
	}
	knownPaths := make(map[string]bool, len(gitWorktrees))
	for _, item := range gitWorktrees {
		knownPaths[resolvePath(item.Path)] = true
	}
	candidates := make([]pruneCandidate, 0, len(reg.Worktrees))
	for _, wt := range reg.Worktrees {
		candidate := pruneCandidate{Worktree: wt, Health: fs.CheckHealth(wt.Path)}
		candidate.GitRecord = knownPaths[resolvePath(wt.Path)]
		if candidate.Health == fs.Healthy && !candidate.GitRecord {
			candidate.Health = fs.NoGitRecord
		}
		if candidate.Health == fs.Healthy {
			status, err := git.InspectWorktreeStatus(resolveWorktreeRemovePath(wt.Path))
			if err != nil {
				candidate.Reason = "could not inspect Git status: " + err.Error()
			} else {
				candidate.Staged = status.Staged
				candidate.Unstaged = status.Unstaged
				candidate.Untracked = status.Untracked
			}
			if candidate.GitRecord {
				ahead, behind, hasUpstream, err := git.AheadBehind(resolveWorktreeRemovePath(wt.Path))
				if err != nil {
					if candidate.Reason == "" {
						candidate.Reason = "could not inspect push status: " + err.Error()
					}
				} else {
					candidate.Ahead, candidate.Behind, candidate.HasUpstream = ahead, behind, hasUpstream
				}
			}
		}
		if candidate.Health != fs.Healthy {
			candidate.Reason = candidate.Health.String()
		} else if candidate.Reason != "" {
			// Preserve inspection failures: unknown safety state must never be
			// rendered as clean.
		} else if candidate.Staged || candidate.Unstaged || candidate.Untracked {
			candidate.Reason = "uncommitted changes"
		} else if candidate.HasUpstream && candidate.Ahead > 0 {
			candidate.Reason = "commits not pushed"
		} else if !candidate.HasUpstream && candidate.GitRecord {
			candidate.Reason = "push status unknown (no upstream)"
		}
		candidate.Safe = candidate.Health == fs.Healthy && candidate.GitRecord &&
			!candidate.Staged && !candidate.Unstaged && !candidate.Untracked &&
			candidate.Reason == "" && candidate.HasUpstream && candidate.Ahead == 0
		candidates = append(candidates, candidate)
	}
	return candidates, nil
}

func removeOrphanedWorktree(repoRoot string, reg *config.Registry, wt config.Worktree, force bool) error {
	if _, err := os.Lstat(wt.Path); err == nil {
		paths, err := pool.DefaultPaths()
		if err != nil {
			return err
		}
		if isPoolResident(wt.Path, paths.MountPoint) {
			if !force {
				confirmed, err := confirmSubvolumeDelete(wt.Name, fs.NoGitRecord)
				if err != nil {
					return err
				}
				if !confirmed {
					return fmt.Errorf("aborted: worktree %q left as-is", wt.Name)
				}
			}
			mounted, err := pool.IsMounted(paths.MountPoint)
			if err != nil {
				return err
			}
			if !mounted {
				return fmt.Errorf("worktree %q points into the Grove pool, but the pool is not mounted; run grove pool init then retry", wt.Name)
			}
			readOnly, err := pool.IsReadOnly(paths.MountPoint)
			if err != nil {
				return err
			}
			if readOnly {
				return fmt.Errorf("Grove pool is read-only; run grove pool repair before pruning %q", wt.Name)
			}
			if err := pool.DeleteSubvolume(wt.Path, paths.MountPoint); err != nil {
				return fmt.Errorf("delete orphaned worktree subvolume %q: %w", wt.Name, err)
			}
		} else {
			return fmt.Errorf("worktree %q has no Git worktree record but its directory still exists at %s; inspect or preserve that directory manually before pruning", wt.Name, wt.Path)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect orphaned worktree %q: %w", wt.Name, err)
	}
	if err := git.WorktreePrune(); err != nil {
		return err
	}
	reg.Remove(wt.Name)
	if err := reg.Save(repoRoot); err != nil {
		return err
	}
	fmt.Printf("%s Removed orphaned worktree %s; branch %s retained.\n", output.Success("✓"), output.Name(wt.Name), wt.Branch)
	return nil
}

func yesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}

// removeNamedWorktree shares grove remove's repair, unhealthy-state recovery,
// and registry cleanup. This preserves the existing branch by design.
func removeNamedWorktree(name string, force bool) error {
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
		return fmt.Errorf("no tracked worktree named %q (see grove list)", name)
	}
	if status := fs.CheckHealth(wt.Path); status != fs.Healthy {
		return removeUnhealthyWorktreeWithForce(repoRoot, reg, wt, status, force)
	}

	// A healthy-looking directory may still be an orphan left behind after
	// its Git administrative record was removed manually. In that case use
	// the orphan cleanup path rather than WorktreeRemove, which requires a
	// live Git worktree record.
	gitWorktrees, err := git.ListWorktrees()
	if err != nil {
		return err
	}
	resolvedRegistryPath := resolvePath(wt.Path)
	hasGitRecord := false
	for _, item := range gitWorktrees {
		if resolvePath(item.Path) == resolvedRegistryPath {
			hasGitRecord = true
			break
		}
	}
	if !hasGitRecord {
		return removeOrphanedWorktree(repoRoot, reg, wt, force)
	}

	resolvedPath := resolveWorktreeRemovePath(wt.Path)
	if err := git.WorktreeRepair(resolvedPath); err != nil {
		return fmt.Errorf("repair worktree metadata for %q before removal: %w", name, err)
	}
	if err := git.WorktreeRemove(resolvedPath, force); err != nil {
		return err
	}
	reg.Remove(name)
	if err := reg.Save(repoRoot); err != nil {
		return err
	}
	fmt.Printf("%s Removed worktree %s (branch %s retained)\n", output.Success("✓"), output.Name(name), wt.Branch)
	return nil
}

// removeUnhealthyWorktreeWithForce is the remove command's existing recovery
// flow parameterized for prune. It deliberately shares all validation and
// pool safety checks, rather than approximating unhealthy cleanup here.
