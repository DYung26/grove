package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/dyung/grove/internal/config"
	"github.com/dyung/grove/internal/fs"
	"github.com/dyung/grove/internal/git"
	"github.com/dyung/grove/internal/output"
	"github.com/spf13/cobra"
)

// unhealthyWorktree pairs a tracked worktree with the real filesystem
// problem CheckHealth found on its directory, so status can report which
// specific thing is wrong rather than lumping every failure into the
// same generic warning.
type unhealthyWorktree struct {
	config.Worktree
	Status fs.HealthStatus
}

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
	unhealthy, err := unhealthyCandidates(reg)
	if err != nil {
		return err
	}

	adoptions, err := adoptCandidates(repoRoot, reg)
	if err != nil {
		return err
	}

	dangling, err := danglingWorktreePaths(repoRoot)
	if err != nil {
		return err
	}

	if len(repairs) == 0 && len(unhealthy) == 0 && len(adoptions) == 0 && len(dangling) == 0 && len(reg.Backups) == 0 {
		fmt.Println(output.Success("✓") + " Everything's in sync — no repair or adopt candidates found.")
		return nil
	}

	printUnhealthyCandidates(unhealthy)
	if len(unhealthy) > 0 && len(repairs) > 0 {
		fmt.Println()
	}
	printRepairCandidates(repairs)
	if (len(unhealthy) > 0 || len(repairs) > 0) && len(adoptions) > 0 {
		fmt.Println()
	}
	printAdoptCandidates(adoptions)
	if (len(unhealthy) > 0 || len(repairs) > 0 || len(adoptions) > 0) && len(dangling) > 0 {
		fmt.Println()
	}
	printDanglingWorktreePaths(dangling)
	if (len(unhealthy) > 0 || len(repairs) > 0 || len(adoptions) > 0 || len(dangling) > 0) && len(reg.Backups) > 0 {
		fmt.Println()
	}
	printBackupSummary(reg.Backups)

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

// unhealthyCandidates returns tracked worktrees that are broken in one
// of two independent ways: a real filesystem-level problem (CheckHealth:
// corrupted, unreadable, or stuck read-only), or a path that's
// completely healthy on disk but has no corresponding entry in `git
// worktree list` at all (fs.NoGitRecord). The second case needs its own
// git.ListWorktrees() cross-reference — CheckHealth is a pure filesystem
// probe and has no way to see it, the same reasoning cmd/prune.go's
// staleWorktrees already documents for its own, separate instance of
// this exact check. Both matter here for the same underlying reason
// repairCandidates being a pure registry read doesn't catch either: a
// worktree can look perfectly fine by every signal `grove status` used
// to consult (registry says reflinked, directory stats and lists fine)
// while still being unusable — `git worktree remove`/`grove remove`
// both refuse to touch a NoGitRecord path, since the git-side record
// they'd normally validate against and clear simply isn't there.
func unhealthyCandidates(reg *config.Registry) ([]unhealthyWorktree, error) {
	gitWorktrees, err := git.ListWorktrees()
	if err != nil {
		return nil, err
	}
	knownPaths := make(map[string]bool, len(gitWorktrees))
	for _, wt := range gitWorktrees {
		knownPaths[resolvePath(wt.Path)] = true
	}

	var candidates []unhealthyWorktree
	for _, wt := range reg.Worktrees {
		status := fs.CheckHealth(wt.Path)
		if status == fs.Healthy && !knownPaths[resolvePath(wt.Path)] {
			status = fs.NoGitRecord
		}
		if status != fs.Healthy {
			candidates = append(candidates, unhealthyWorktree{Worktree: wt, Status: status})
		}
	}
	return candidates, nil
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

// danglingWorktreePaths scans the sibling <repo>.wt container directory
// (the same location resolveWorktreesDir in create.go writes new
// worktrees into) for entries that are broken symlinks — orphaned
// remnants of a pool migration whose subvolume was later deleted (e.g.
// by `grove pool repair`'s Recreate, or a manual `btrfs subvolume
// delete`) without the symlink pointing at it ever being cleaned up.
//
// This is a distinct blind spot from both adoptCandidates and
// unhealthyCandidates: it isn't a git worktree at all (git worktree add
// refuses on a path collision without ever creating one, so `git
// worktree list` never knows it exists) and it isn't a tracked registry
// entry either, so nothing else in `grove status` ever looks at it. Left
// undetected, the first anyone learns of it is `grove create` failing
// with git's own "already exists" on whatever branch happens to hash to
// the same worktree name — see requireFreeWorktreePath in create.go,
// which performs the same Lstat-based check reactively, at the one
// specific path a create is about to use, right before failing outright
// rather than proceeding past it. Skipped entirely if the .wt directory
// doesn't exist yet (nothing created here so far) or can't be listed for
// some other reason — this is a bonus scan on top of `grove status`'s
// core registry-based checks, not something that should fail the whole
// command.
func danglingWorktreePaths(repoRoot string) ([]string, error) {
	wtDir := filepath.Join(filepath.Dir(repoRoot), filepath.Base(repoRoot)+".wt")

	entries, err := os.ReadDir(wtDir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, nil
	}

	var dangling []string
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink == 0 {
			continue
		}
		path := filepath.Join(wtDir, entry.Name())
		if _, err := os.Stat(path); os.IsNotExist(err) {
			dangling = append(dangling, path)
		}
	}
	return dangling, nil
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

func printUnhealthyCandidates(candidates []unhealthyWorktree) {
	if len(candidates) == 0 {
		return
	}

	fmt.Println(output.Bold("Unhealthy worktrees") + " (directory problems, not just reflink drift):")
	for _, wt := range candidates {
		fmt.Printf("  %s\t(%s)\n", describeListEntry(wt.Worktree), output.Warn(wt.Status.String()))
		fmt.Println("    " + unhealthyFixMessage(wt.Status))
	}
}

// unhealthyFixMessage explains what a given HealthStatus actually means
// and the right next command, rather than one generic line shared across
// every status. Missing in particular reads very differently from
// Unreadable/ReadOnly: those describe a subvolume that's still there but
// broken in place, so `grove remove` deleting "just that subvolume" is
// literally what happens. Missing means the subvolume itself is already
// gone (most commonly because `grove pool repair` recreated the whole
// pool image, which doesn't touch the registry) — there's nothing left
// for `grove remove` to delete, only a stale registry entry and a
// dangling git worktree record to clean up, and telling the user that
// distinction up front avoids them expecting a destructive delete
// confirmation that will never come.
func unhealthyFixMessage(status fs.HealthStatus) string {
	switch status {
	case fs.Missing:
		return "the subvolume backing this worktree is gone (likely from `grove pool repair` recreating the pool) — run " + output.Command("grove remove <name>") + " to untrack it (nothing left to delete, just cleans up the stale registry/git entries), then " + output.Command("grove create <branch>") + " for a fresh one"
	case fs.ReadOnly:
		return "the pool this worktree lives in is stuck read-only — run " + output.Command("grove pool repair") + " first, then re-check with " + output.Command("grove status")
	case fs.NoGitRecord:
		return "the directory (or pool subvolume) is intact, but git no longer has a worktree record for it — likely removed outside Grove (a bare `git worktree remove`/`git worktree prune` run by hand). " + output.Command("grove remove <name>") + " will fail here since it needs a live git record to validate against; run " + output.Command("grove prune") + " instead, which detects this exact case and clears the stale registry entry (and, if pool-resident, the subvolume underneath it stays orphaned — check " + output.Command("grove pool status") + " afterward)"
	default: // fs.Unreadable
		return "fix with: " + output.Command("grove remove <name>") + " (deletes just that subvolume, if pool-resident) or investigate the underlying filesystem manually otherwise"
	}
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

// printDanglingWorktreePaths reports broken symlinks found directly in
// the .wt container — orphaned pool-migration remnants that block
// `grove create` from ever writing to that exact path again, but were
// otherwise invisible (not a git worktree, not a registry entry; see
// danglingWorktreePaths for why neither adoptCandidates nor
// unhealthyCandidates can see them).
func printDanglingWorktreePaths(paths []string) {
	if len(paths) == 0 {
		return
	}

	fmt.Println(output.Bold("Dangling worktree paths") + " (broken symlinks, likely orphaned pool migrations — not tracked, not a git worktree, just blocking that path):")
	for _, path := range paths {
		fmt.Printf("  %s\n", output.Path(path))
	}
	fmt.Println("  fix with: " + output.Commandf("rm <path>") + " once confirmed nothing needs it (check " + output.Command("grove pool status") + " first if the target subvolume might still exist)")
}

// printBackupSummary reports how many .grove-bak backups are on record
// and roughly how old the oldest one is, without walking the filesystem
// or listing every entry individually — `grove status` is meant to stay
// a quick scan, and per-entry detail (age, source, size) belongs to
// `grove cleanup`, which is also where the count here points the user
// next.
func printBackupSummary(backups []config.BackupEntry) {
	if len(backups) == 0 {
		return
	}

	oldest := backups[0].CreatedAt
	for _, b := range backups[1:] {
		if b.CreatedAt.Before(oldest) {
			oldest = b.CreatedAt
		}
	}

	fmt.Println(output.Bold("Backups") + fmt.Sprintf(" (%d .grove-bak director%s left by repair/pool migrations, oldest from %s ago):",
		len(backups), pluralY(len(backups)), formatAge(time.Since(oldest))))
	fmt.Println("  review with: " + output.Command("grove cleanup"))
}

// pluralY picks between "y" and "ies" for a preceding "director" so the
// summary reads correctly for both a single backup and several.
func pluralY(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}
