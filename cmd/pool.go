package cmd

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/dyung/grove/internal/config"
	"github.com/dyung/grove/internal/git"
	"github.com/dyung/grove/internal/output"
	"github.com/dyung/grove/internal/pool"
	"github.com/spf13/cobra"
)

var poolInitSizeGB uint64

var poolCmd = &cobra.Command{
	Use:   "pool",
	Short: "Manage the Btrfs loopback pool used for CoW clones on non-CoW filesystems",
}

var poolInitCmd = &cobra.Command{
	Use:   "init",
	Short: "Create and mount the Btrfs loopback pool, and move the current repo's dependency dirs into it",
	RunE:  runPoolInit,
}

var poolStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show whether the pool is mounted/writable, and flag this repo's orphaned subvolumes",
	RunE:  runPoolStatus,
}

var poolMigrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "One-time migration: convert this repo's whole-container pool volume into per-worktree subvolumes",
	RunE:  runPoolMigrate,
}

var poolRepairForce bool

var poolRepairCmd = &cobra.Command{
	Use:   "repair",
	Short: "Recover a pool that btrfs has forced read-only due to metadata corruption",
	RunE:  runPoolRepair,
}

func init() {
	poolInitCmd.Flags().Uint64Var(&poolInitSizeGB, "size", pool.DefaultSizeGB, "pool size in GiB")
	poolRepairCmd.Flags().BoolVar(&poolRepairForce, "force", false, "skip confirmation before recreating the pool")
	poolCmd.AddCommand(poolInitCmd, poolStatusCmd, poolMigrateCmd, poolRepairCmd)
	rootCmd.AddCommand(poolCmd)
}

func runPoolInit(cmd *cobra.Command, args []string) error {
	paths, err := pool.DefaultPaths()
	if err != nil {
		return err
	}

	sizeGB, err := resolvePoolSize(paths, poolInitSizeGB, pool.Init)
	if err != nil {
		return err
	}
	if sizeGB == 0 {
		// User declined the suggested smaller size; told them how to retry, nothing left to do.
		return nil
	}

	return migrateMainDependencyDirsIntoPool(paths)
}

// resolvePoolSize runs op(paths, sizeGB) and, if it fails specifically
// because the requested size exceeds available space, offers the
// caller's computed maximum as a fallback instead of just surfacing the
// error. This is shared between `grove pool init` and the recreate step
// inside `grove pool repair`, since both ultimately call pool.Init with
// a user- or flag-supplied size and both should degrade the same way
// when that size doesn't fit.
//
// Returns the size that was actually applied (0 if the user declined the
// fallback, which callers treat as "nothing more to do here" rather than
// an error, since declining isn't a failure — it's a valid choice to go
// rerun the command with an explicit --size instead).
func resolvePoolSize(paths pool.Paths, requestedGB uint64, op func(pool.Paths, uint64) error) (uint64, error) {
	err := op(paths, requestedGB)
	if err == nil {
		return requestedGB, nil
	}

	var sizeErr *pool.SizeExceedsLimitError
	if !errors.As(err, &sizeErr) {
		return 0, err
	}

	if sizeErr.MaxGB == 0 {
		return 0, fmt.Errorf("%w (not enough free space for any pool size; free up space first)", err)
	}

	fmt.Println(output.Warn(err.Error()))
	fmt.Printf("Use %dG instead (the largest size that currently fits)? [Y/n] ", sizeErr.MaxGB)

	confirmed, readErr := confirmYesDefault()
	if readErr != nil {
		return 0, readErr
	}
	if !confirmed {
		fmt.Printf("Okay — re-run with %s once you've freed up space or decided on a size.\n", output.Command(fmt.Sprintf("--size %d", sizeErr.MaxGB)))
		return 0, nil
	}

	if err := op(paths, sizeErr.MaxGB); err != nil {
		return 0, err
	}
	return sizeErr.MaxGB, nil
}

// confirmYesDefault is the [Y/n] counterpart to confirmPoolRecreate's
// [y/N]: empty input (just pressing enter) counts as yes here, since the
// fallback size being offered is already the safe, computed-to-fit
// choice rather than a destructive action that should default to no.
func confirmYesDefault() (bool, error) {
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil {
		return false, err
	}

	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "" || answer == "y" || answer == "yes", nil
}

// migrateMainDependencyDirsIntoPool moves the main repo's own dependency
// dirs (node_modules, target, .venv, etc.) into the pool, symlinked back
// into place, so they become a valid reflink source for every worktree
// created from now on. Without this, the main repo's copy stays on its
// original (often non-CoW) filesystem forever, forcing every worktree to
// hardlink or fully copy from it instead of reflinking — reflink can
// never cross filesystems, pool or no pool.
//
// Run once, as part of `grove pool init`, rather than lazily inside
// `grove create`: relocating real files the user didn't explicitly ask to
// move is significant enough to be its own visible step, not a side
// effect of creating a worktree. If `grove pool init` is run outside any
// repo (a one-time, machine-wide setup is a legitimate use case), this
// step is skipped rather than failing the whole command.
func migrateMainDependencyDirsIntoPool(paths pool.Paths) error {
	repoRoot, err := git.MainRepoRoot()
	if err != nil {
		fmt.Println(output.Dim("Not inside a git repo — skipping dependency-dir migration (run `grove pool init` again from inside a repo to enable reflinking its dependency dirs)."))
		return nil
	}

	dirs, err := resolveRepoDependencyDirs(repoRoot)
	if err != nil {
		return err
	}

	repoName := filepath.Base(repoRoot)
	for _, dir := range dirs {
		if _, err := os.Stat(filepath.Join(repoRoot, dir)); os.IsNotExist(err) {
			continue
		}

		_, backup, err := pool.EnsureDependencyDirVolume(paths.MountPoint, repoName, "main", repoRoot, dir)
		if err != nil {
			return fmt.Errorf("move %s into the pool: %w", dir, err)
		}
		if err := recordBackupIfAny(repoRoot, backup, "pool-migrate"); err != nil {
			return err
		}
	}
	return nil
}

// runPoolMigrate is a one-time fixup for repos provisioned by the
// retired EnsureRepoVolume path (a single subvolume for the whole
// <repo>.wt container), converting them to Grove's per-worktree layout
// so `grove repair`/`grove adopt` no longer need to special-case them.
// Worktree paths recorded in the registry don't change — only what's on
// disk at <repo>.wt/<name> does — so no registry update is needed here.
func runPoolMigrate(cmd *cobra.Command, args []string) error {
	repoRoot, err := git.MainRepoRoot()
	if err != nil {
		return err
	}

	reg, err := config.Load(repoRoot)
	if err != nil {
		return err
	}

	paths, err := pool.DefaultPaths()
	if err != nil {
		return err
	}

	mounted, err := pool.IsMounted(paths.MountPoint)
	if err != nil {
		return err
	}
	if !mounted {
		return fmt.Errorf("pool isn't mounted (run %s first)", output.Command("grove pool init"))
	}

	migrated, backup, err := pool.SplitRepoVolume(paths.MountPoint, repoRoot, trackedWorktreeNames(reg))
	if err != nil {
		return err
	}
	if !migrated {
		fmt.Println(output.Dim("Nothing to migrate: this repo isn't on the old whole-container layout."))
		return nil
	}
	if err := recordBackupIfAny(repoRoot, backup, "split-repo-volume"); err != nil {
		return err
	}

	fmt.Printf("%s Migrated %s to per-worktree subvolumes\n", output.Success("✓"), output.Name(filepath.Base(repoRoot)+".wt"))
	return nil
}

// trackedWorktreeNames returns the registry's worktree names, the same
// identifiers SplitRepoVolume matches against top-level entries in the
// old container volume to tell an actual Grove worktree apart from
// incidental data (e.g. a Syncthing .stfolder marker) that happened to
// live alongside them.
func trackedWorktreeNames(reg *config.Registry) []string {
	names := make([]string, len(reg.Worktrees))
	for i, wt := range reg.Worktrees {
		names[i] = wt.Name
	}
	return names
}

// runPoolRepair recovers a pool btrfs has force-remounted read-only.
// Tries the non-destructive fix first — a plain remount,rw — since a
// transient issue (rather than ongoing metadata corruption) can
// sometimes clear on remount. If the pool immediately re-trips back to
// read-only, that confirms the corruption is real and not transient, and
// the only remaining fix is to recreate the pool image from scratch,
// which is confirmed with the user first (unless --force) since it
// discards every worktree's reflinked dependency dirs and pool-resident
// container currently in the pool, not just the one that was originally
// found corrupted.
func runPoolRepair(cmd *cobra.Command, args []string) error {
	paths, err := pool.DefaultPaths()
	if err != nil {
		return err
	}

	mounted, err := pool.IsMounted(paths.MountPoint)
	if err != nil {
		return err
	}
	if !mounted {
		fmt.Println(output.Dim("Pool isn't mounted; nothing to repair (run `grove pool init` if you want it set up)."))
		return nil
	}

	readOnly, err := pool.IsReadOnly(paths.MountPoint)
	if err != nil {
		return err
	}
	if !readOnly {
		fmt.Println(output.Dim("Pool is mounted read-write; nothing to repair."))
		return nil
	}

	fmt.Println("Pool is read-only. Trying a plain remount first...")
	if err := pool.RemountReadWrite(paths.MountPoint); err != nil {
		fmt.Println(output.Warn("Remount itself failed (not just re-tripped read-only): ") + err.Error())
	}

	stillReadOnly, err := pool.IsReadOnly(paths.MountPoint)
	if err != nil {
		return err
	}
	if !stillReadOnly {
		fmt.Printf("%s Pool is writable again — the read-only trip was transient, not ongoing corruption.\n", output.Success("✓"))
		return nil
	}

	fmt.Println(output.Warn("Pool re-tripped straight back to read-only — this is real metadata corruption, not a transient issue."))
	if !poolRepairForce {
		confirmed, err := confirmPoolRecreate()
		if err != nil {
			return err
		}
		if !confirmed {
			return fmt.Errorf("aborted: the pool is still read-only; re-run `grove pool repair --force` when you're ready, or investigate manually first (e.g. `sudo btrfs check %s` while unmounted)", paths.Image)
		}
	}

	fmt.Println("Recreating the pool from scratch...")
	sizeGB, err := resolvePoolSize(paths, poolInitSizeGB, pool.Recreate)
	if err != nil {
		return fmt.Errorf("recreate pool: %w", err)
	}
	if sizeGB == 0 {
		// User declined the suggested smaller size; the pool is left recreated at whatever state Recreate's initial attempt left it in (still absent/unmounted, since Init never got past the space check), and they've been told how to retry.
		return nil
	}

	fmt.Printf("%s Pool recreated. Every tracked worktree's dependency dirs and pool-resident container need re-cloning: run %s for each (see `grove status` for the list).\n",
		output.Success("✓"), output.Command("grove repair <n>"))
	return nil
}

// confirmPoolRecreate asks before wiping the pool, the same [y/N]
// pattern confirmPlainFallback (create.go) uses for a lower-stakes
// choice — this one is more consequential, so it also spells out exactly
// what's about to be lost rather than assuming the user already knows.
func confirmPoolRecreate() (bool, error) {
	fmt.Println(output.Warn("This will destroy the entire pool image and rebuild it empty."))
	fmt.Println("Every worktree's reflinked dependency dirs, and any worktree containers migrated into the pool, will be lost —")
	fmt.Println("but this is safe: everything in the pool is a disposable CoW clone. Branches and commit history live in git, untouched.")
	fmt.Print("Recreate the pool? [y/N] ")

	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil {
		return false, err
	}

	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes", nil
}

func runPoolStatus(cmd *cobra.Command, args []string) error {
	paths, err := pool.DefaultPaths()
	if err != nil {
		return err
	}

	mounted, err := pool.IsMounted(paths.MountPoint)
	if err != nil {
		return err
	}

	if !mounted {
		fmt.Println(output.Warn("pool not mounted") + " (run " + output.Command("grove pool init") + ")")
		return nil
	}

	readOnly, err := pool.IsReadOnly(paths.MountPoint)
	if err != nil {
		return err
	}
	if readOnly {
		fmt.Printf("%s pool mounted %s at %s — btrfs has stopped accepting writes, almost certainly due to metadata corruption. Nothing can be created or deleted inside it (including unrelated new worktrees) until this is fixed. Run %s.\n",
			output.Warn("⚠"), output.Warn("read-only"), output.Path(paths.MountPoint), output.Command("grove pool repair"))
		return nil
	}

	fmt.Printf("%s pool mounted at %s\n", output.Success("✓"), output.Path(paths.MountPoint))
	return reportOrphanSubvolumes(paths.MountPoint)
}

// reportOrphanSubvolumes lists every top-level subvolume actually in the
// pool and flags any this repo's registry no longer references — e.g.
// left behind by an interrupted operation, or by a symlink that was
// deleted or repointed without the subvolume underneath it ever being
// cleaned up via `grove remove`/DeleteSubvolume. This is read-only
// reporting, same contract as the rest of `grove status`/`grove pool
// status`: it names what it found and what command would fix it, and
// does not delete anything itself.
//
// The pool is machine-wide (~/.grove/pool.img) but the registry is
// per-repo (.grove/registry.json), so this can only ever check
// subvolumes belonging to *this* repo (identified by its name prefix on
// the subvolume names EnsureWorktreeVolume/EnsureDependencyDirVolume
// construct) against what *this* repo's registry says should exist.
// Subvolumes prefixed with some other repo's name are silently skipped
// rather than misreported as orphans of a repo they don't belong to —
// checking those would need that other repo's own registry, which isn't
// available from here. Run from outside any repo, or if the pool itself
// can't be listed, this degrades to a note rather than failing the
// whole command, since orphan-checking is a bonus on top of the mount/
// read-only check above, not the reason someone runs `grove pool
// status` in the first place.
func reportOrphanSubvolumes(mountPoint string) error {
	repoRoot, err := git.MainRepoRoot()
	if err != nil {
		fmt.Println(output.Dim("Not inside a git repo — skipping the orphaned-subvolume check (run from inside a repo to check its subvolumes)."))
		return nil
	}

	subvolumes, err := pool.ListSubvolumes(mountPoint)
	if err != nil {
		fmt.Println(output.Warn("Note: couldn't list pool subvolumes to check for orphans: ") + err.Error())
		return nil
	}

	reg, err := config.Load(repoRoot)
	if err != nil {
		return err
	}

	repoName := filepath.Base(repoRoot)
	expected := expectedSubvolumeNames(repoRoot, repoName, reg)

	prefix := repoName + "-"
	var orphans []string
	for _, name := range subvolumes {
		if !strings.HasPrefix(name, prefix) {
			continue // belongs to a different repo (or isn't repo-scoped at all); not this registry's to judge
		}
		if !expected[name] {
			orphans = append(orphans, name)
		}
	}

	if len(orphans) == 0 {
		return nil
	}

	fmt.Println()
	fmt.Println(output.Bold("Orphaned subvolumes") + fmt.Sprintf(" (in the pool, prefixed %q, but not referenced by any tracked worktree in this repo):", prefix))
	for _, name := range orphans {
		fmt.Printf("  %s\n", output.Path(filepath.Join(mountPoint, name)))
	}
	fmt.Println("  these are most likely leftover from an interrupted operation or a symlink that got deleted/repointed manually — verify with " + output.Command("sudo btrfs subvolume show <path>") + ", then remove with " + output.Command("sudo btrfs subvolume delete <path>") + " once you've confirmed nothing needs it")
	return nil
}

// expectedSubvolumeNames reconstructs the volumeName strings
// EnsureWorktreeVolume/EnsureDependencyDirVolume would have used for
// every tracked worktree's own container and each of its resolved
// dependency dirs, forming the "known good" set reportOrphanSubvolumes
// checks the pool's actual contents against. Rebuilds the naming
// convention ("<repo>-<label>", "<repo>-<label>-<depdir>") rather than
// exporting a shared helper from internal/pool, since this needs the
// pattern, not a filesystem check against any specific existing path —
// an orphan by definition has no live symlink left pointing at it for a
// resolve-through-symlink approach to follow.
func expectedSubvolumeNames(repoRoot, repoName string, reg *config.Registry) map[string]bool {
	expected := make(map[string]bool)
	expected[fmt.Sprintf("%s-main", repoName)] = true

	dirs, err := resolveRepoDependencyDirs(repoRoot)
	if err != nil {
		dirs = nil // best-effort: an unreadable .grove.json shouldn't block the rest of the check
	}
	for _, dir := range dirs {
		expected[fmt.Sprintf("%s-main-%s", repoName, filepath.Base(dir))] = true
	}

	for _, wt := range reg.Worktrees {
		expected[fmt.Sprintf("%s-%s", repoName, wt.Name)] = true
		for _, dir := range dirs {
			expected[fmt.Sprintf("%s-%s-%s", repoName, wt.Name, filepath.Base(dir))] = true
		}
	}
	return expected
}
