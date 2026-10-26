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

var cleanForce bool

var cleanCmd = &cobra.Command{
	Use:               "clean [name]",
	ValidArgsFunction: completeWorktreeNames,
	Short:             "Review and remove worktrees that are no longer wanted",
	Long:              "Without a name, inspect tracked worktrees and interactively choose one to remove. With a name, remove that worktree directly. The inventory shows filesystem health, staged/unstaged/untracked changes, and upstream status. Branch deletion is offered after worktree removal.",
	Args:              cobra.MaximumNArgs(1),
	RunE:              runClean,
}

func init() {
	cleanCmd.Flags().BoolVar(&cleanForce, "force", false, "skip worktree removal confirmations and allow removal despite uncommitted changes")
	rootCmd.AddCommand(cleanCmd)
}

func runClean(cmd *cobra.Command, args []string) error {
	if len(args) == 1 {
		return cleanNamedWorktree(args[0], cleanForce)
	}
	if cleanForce {
		return fmt.Errorf("--force requires a worktree name: grove clean <name> --force")
	}
	return runInteractiveClean()
}

func cleanNamedWorktree(name string, force bool) error {
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
	if err := removeNamedWorktree(name, force); err != nil {
		return err
	}
	return removeBranchIfWanted(wt.Branch)
}

// cleanCandidate is the read-only inventory displayed by grove clean.
type cleanCandidate struct {
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

// runInteractiveClean inventories registry entries, lets the user choose one,
// then delegates actual deletion to the same recovery-aware remove path as
// grove remove. Nothing is mutated while building or displaying the inventory.
func runInteractiveClean() error {
	repoRoot, err := git.MainRepoRoot()
	if err != nil {
		return err
	}
	reg, err := config.Load(repoRoot)
	if err != nil {
		return err
	}
	candidates, err := inspectCleanCandidates(reg)
	if err != nil {
		return err
	}
	if len(candidates) == 0 {
		fmt.Println("No tracked worktrees to clean.")
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
	fmt.Printf("Delete worktree %q (%s)? [y/N] ", selected.Worktree.Name, selected.Worktree.Branch)
	answer, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if !strings.EqualFold(strings.TrimSpace(answer), "y") && !strings.EqualFold(strings.TrimSpace(answer), "yes") {
		fmt.Println("Cancelled; no worktrees changed.")
		return nil
	}
	return cleanNamedWorktree(selected.Worktree.Name, false)
}

func inspectCleanCandidates(reg *config.Registry) ([]cleanCandidate, error) {
	gitWorktrees, err := git.ListWorktrees()
	if err != nil {
		return nil, err
	}
	knownPaths := make(map[string]bool, len(gitWorktrees))
	for _, item := range gitWorktrees {
		knownPaths[resolvePath(item.Path)] = true
	}
	candidates := make([]cleanCandidate, 0, len(reg.Worktrees))
	for _, wt := range reg.Worktrees {
		candidate := cleanCandidate{Worktree: wt, Health: fs.CheckHealth(wt.Path)}
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
				return fmt.Errorf("Grove pool is read-only; run grove pool repair before cleaning %q", wt.Name)
			}
			if err := pool.DeleteSubvolume(wt.Path, paths.MountPoint); err != nil {
				return fmt.Errorf("delete orphaned worktree subvolume %q: %w", wt.Name, err)
			}
		} else {
			return fmt.Errorf("worktree %q has no Git worktree record but its directory still exists at %s; inspect or preserve that directory manually before cleaning", wt.Name, wt.Path)
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
	fmt.Printf("%s Removed orphaned worktree %s.\n", output.Success("✓"), output.Name(wt.Name))
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
	fmt.Printf("%s Removed worktree %s\n", output.Success("✓"), output.Name(name))
	return nil
}

// removeUnhealthyWorktreeWithForce is the remove command's existing recovery
// flow parameterized for clean. It deliberately shares all validation and
// pool safety checks, rather than approximating unhealthy cleanup here.
