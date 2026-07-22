package cmd

import (
	"fmt"
	"path/filepath"

	"github.com/dyung/grove/internal/config"
	"github.com/dyung/grove/internal/fs"
	"github.com/dyung/grove/internal/git"
	"github.com/dyung/grove/internal/output"
	"github.com/spf13/cobra"
)

var statusCmd = &cobra.Command{
	Use:     "status",
	Aliases: []string{"doctor"},
	Short:   "Report worktrees that have drifted from Grove's expectations",
	RunE:    runStatus,
}

func init() {
	rootCmd.AddCommand(statusCmd)
}

// runStatus scans for two kinds of drift between the registry and what's
// actually on disk: tracked worktrees whose dependency dirs aren't (or
// are no longer) reflinked (grove repair candidates), and git worktrees
// that exist but aren't tracked at all (grove adopt candidates). Neither
// is fixed automatically — this command only reports.
func runStatus(cmd *cobra.Command, args []string) error {
	repoRoot, err := git.MainRepoRoot()
	if err != nil {
		return err
	}

	reg, err := config.Load(repoRoot)
	if err != nil {
		return err
	}

	repairs := repairCandidates(reg)

	adoptions, err := adoptCandidates(repoRoot, reg)
	if err != nil {
		return err
	}

	if len(repairs) == 0 && len(adoptions) == 0 {
		fmt.Println(output.Success("✓") + " Everything's in sync — no repair or adopt candidates found.")
		return nil
	}

	printRepairCandidates(repairs)
	if len(repairs) > 0 && len(adoptions) > 0 {
		fmt.Println()
	}
	printAdoptCandidates(adoptions)

	return nil
}

// repairCandidates returns tracked worktrees whose dependency dirs aren't
// fully reflinked — anything other than CloneModeReflink, including the
// "" left by grove adopt (whose history isn't recoverable) and by
// worktrees created before this field existed. Worktrees whose project
// type has no dependency dirs at all (config.DepsCloneModeNone) are
// excluded — there's nothing for `grove repair` to ever fix there, so
// listing them would just be permanent, unactionable noise.
func repairCandidates(reg *config.Registry) []config.Worktree {
	var candidates []config.Worktree
	for _, wt := range reg.Worktrees {
		if wt.DepsCloneMode != fs.CloneModeReflink.String() && wt.DepsCloneMode != config.DepsCloneModeNone {
			candidates = append(candidates, wt)
		}
	}
	return candidates
}

// adoptCandidates returns git worktrees that exist on disk but aren't in
// the registry at all — either created before Grove, or with a plain
// `git worktree add` that bypassed Grove entirely. The main repo's own
// working tree and bare/detached worktrees (no branch to key a registry
// entry on, same restriction grove adopt enforces) are excluded.
//
// Matching is done by Path, not Branch, since Branch can diverge from
// Grove's own Name after a rename (see resolveWorktreeName in
// create.go) and would otherwise false-positive a tracked worktree as
// adoptable. Both sides are resolved through symlinks first: a tracked
// Path can be a symlink into the Grove pool (EnsureWorktreeVolume in
// internal/pool/repo.go) while `git worktree list` reports the real path
// underneath, so comparing literal strings would flag a pool-backed
// worktree as an adopt candidate forever.
func adoptCandidates(repoRoot string, reg *config.Registry) ([]git.WorktreeInfo, error) {
	worktrees, err := git.ListWorktrees()
	if err != nil {
		return nil, err
	}

	trackedPaths := make(map[string]bool, len(reg.Worktrees))
	for _, wt := range reg.Worktrees {
		trackedPaths[resolvePath(wt.Path)] = true
	}

	var candidates []git.WorktreeInfo
	for _, wt := range worktrees {
		if wt.Path == repoRoot || wt.Branch == "" || trackedPaths[resolvePath(wt.Path)] {
			continue
		}
		candidates = append(candidates, wt)
	}
	return candidates, nil
}

// resolvePath resolves path through any symlinks, returning path
// unchanged if it doesn't exist or isn't a symlink — the same fallback
// isPoolResident (create.go) uses, so a not-yet-existing or plain path
// still compares correctly.
func resolvePath(path string) string {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return path
	}
	return resolved
}

func printRepairCandidates(candidates []config.Worktree) {
	if len(candidates) == 0 {
		return
	}

	fmt.Println(output.Bold("Repair candidates") + " (dependency dirs not fully reflinked):")
	for _, wt := range candidates {
		mode := wt.DepsCloneMode
		if mode == "" {
			mode = "unknown"
		}
		fmt.Printf("  %s\t(%s)\n", describeListEntry(wt), output.Warn(mode))
	}
	fmt.Println("  fix with: " + output.Command("grove repair <name>"))
}

func printAdoptCandidates(candidates []git.WorktreeInfo) {
	if len(candidates) == 0 {
		return
	}

	fmt.Println(output.Bold("Adopt candidates") + " (git worktrees Grove isn't tracking):")
	for _, wt := range candidates {
		fmt.Printf("  branch %s\t%s\n", wt.Branch, output.Path(wt.Path))
	}
	fmt.Println("  fix with: " + output.Command("grove adopt <path>"))
}
