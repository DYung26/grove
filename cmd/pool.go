package cmd

import (
	"fmt"
	"os"
	"path/filepath"

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
	Short: "Show whether the Btrfs loopback pool is mounted",
	RunE:  runPoolStatus,
}

var poolMigrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "One-time migration: convert this repo's whole-container pool volume into per-worktree subvolumes",
	RunE:  runPoolMigrate,
}

func init() {
	poolInitCmd.Flags().Uint64Var(&poolInitSizeGB, "size", pool.DefaultSizeGB, "pool size in GiB")
	poolCmd.AddCommand(poolInitCmd, poolStatusCmd, poolMigrateCmd)
	rootCmd.AddCommand(poolCmd)
}

func runPoolInit(cmd *cobra.Command, args []string) error {
	paths, err := pool.DefaultPaths()
	if err != nil {
		return err
	}
	if err := pool.Init(paths, poolInitSizeGB); err != nil {
		return err
	}

	return migrateMainDependencyDirsIntoPool(paths)
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

		if _, err := pool.EnsureDependencyDirVolume(paths.MountPoint, repoName, "main", repoRoot, dir); err != nil {
			return fmt.Errorf("move %s into the pool: %w", dir, err)
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

	migrated, err := pool.SplitRepoVolume(paths.MountPoint, repoRoot, trackedWorktreeNames(reg))
	if err != nil {
		return err
	}
	if !migrated {
		fmt.Println(output.Dim("Nothing to migrate: this repo isn't on the old whole-container layout."))
		return nil
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

func runPoolStatus(cmd *cobra.Command, args []string) error {
	paths, err := pool.DefaultPaths()
	if err != nil {
		return err
	}

	mounted, err := pool.IsMounted(paths.MountPoint)
	if err != nil {
		return err
	}

	if mounted {
		fmt.Printf("%s pool mounted at %s\n", output.Success("✓"), output.Path(paths.MountPoint))
	} else {
		fmt.Println(output.Warn("pool not mounted") + " (run " + output.Command("grove pool init") + ")")
	}
	return nil
}
