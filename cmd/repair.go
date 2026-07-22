package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/dyung/grove/internal/config"
	"github.com/dyung/grove/internal/fs"
	"github.com/dyung/grove/internal/git"
	"github.com/dyung/grove/internal/output"
	"github.com/dyung/grove/internal/pool"
	"github.com/dyung/grove/internal/project"
	"github.com/spf13/cobra"
)

var repairForce bool
var repairFrom string

var repairCmd = &cobra.Command{
	Use:     "repair <name>",
	Aliases: []string{"sync-deps"},
	Short:   "Retrofit a worktree onto the pool (container and dependency dirs) so it can actually reflink",
	Args:    cobra.ExactArgs(1),
	RunE:    runRepair,
}

func init() {
	repairCmd.Flags().BoolVar(&repairForce, "force", false, "re-clone even if the registry already says deps are reflinked, and skip the .bak safety rename")
	repairCmd.Flags().StringVar(&repairFrom, "from", "", "clone dependency dirs from this worktree (name or path) instead of the main repo; migrates it into the pool first if needed so it can actually be reflinked from")
	rootCmd.AddCommand(repairCmd)
}

// runRepair retrofits an already-tracked worktree onto the pool so its
// dependency dirs can actually reflink: first the worktree's own
// container directory, if it's still a plain directory on a filesystem
// that can't reflink (see ensureWorktreePoolResident), then its
// dependency dirs, replacing plain copies or hardlinks with reflinks
// where the destination now supports them. Covers a worktree created
// before the pool existed, before CoW support landed for its project
// type, or adopted from a pre-Grove layout (DepsCloneMode == "" is
// treated the same as a known-non-reflink mode: worth checking).
func runRepair(cmd *cobra.Command, args []string) error {
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

	if wt.DepsCloneMode == fs.CloneModeReflink.String() && !repairForce {
		fmt.Printf("%s's dependency dirs are already reflinked; nothing to do (use --force to re-clone anyway)\n", output.Name(name))
		return nil
	}
	if wt.DepsCloneMode == config.DepsCloneModeNone {
		fmt.Printf("%s's project type has no dependency dirs to manage; nothing to do\n", output.Name(name))
		return nil
	}

	if _, err := os.Stat(wt.Path); err != nil {
		return fmt.Errorf("worktree %q is tracked but its directory is missing: %w", name, err)
	}

	if err := ensureWorktreePoolResident(repoRoot, name, wt.Path); err != nil {
		return err
	}

	depsSrcRoot, err := resolveDepsSourceRoot(repoRoot, repairFrom)
	if err != nil {
		return err
	}

	depsCloneMode, err := repairDependencyDirs(repoRoot, depsSrcRoot, wt.Path)
	if err != nil {
		return err
	}

	wt.DepsCloneMode = depsCloneMode
	reg.Update(wt)
	if err := reg.Save(repoRoot); err != nil {
		return err
	}

	fmt.Printf("%s Repaired %s\n", output.Success("✓"), output.Name(name))
	return nil
}

// ensureWorktreePoolResident migrates worktreePath itself into the pool
// if it's a plain directory on a filesystem that can't reflink and the
// pool is mounted. `grove create` calls this on every new worktree right
// after `git worktree add`; `grove repair` calls it here as a retrofit
// for a worktree that never went through it — created before the pool
// existed, or adopted from a pre-Grove layout. Either way, repairing
// only a worktree's *dependency dirs* can never make them reflink-able
// if the worktree's own container is still sitting on a different
// filesystem than the pool, since reflink can never cross filesystems
// regardless of how pool-resident the dependency-dir source is.
//
// No-ops when the worktree's filesystem already supports reflink
// natively (nothing to migrate), or when the pool isn't mounted at all
// (nothing to migrate into yet — the existing hardlink/copy fallback
// prompt still applies in that case, same as always).
func ensureWorktreePoolResident(repoRoot, name, worktreePath string) error {
	if fs.DetectCloneSupport(filepath.Dir(worktreePath)) == fs.CloneSupported {
		return nil
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
		return nil
	}

	// EnsureWorktreeVolume already Lstats worktreePath itself and no-ops if
	// it's already a symlink (i.e. already migrated), so no need to check
	// that here first.
	repoName := filepath.Base(repoRoot)
	if _, err := pool.EnsureWorktreeVolume(paths.MountPoint, repoName, name, worktreePath); err != nil {
		return fmt.Errorf("move worktree %s into the pool: %w", name, err)
	}
	return nil
}

// repairDependencyDirs re-clones each resolved dependency dir from
// srcRoot into worktreePath, in place of whatever's already there. The
// dependency-dir *list* still resolves from repoRoot's .grove.json, same
// split as cloneDependencyDirs in create.go, since that config is a
// property of the main repo regardless of which worktree --from points
// the clone source at. Existing dir contents are renamed to
// <dir>.bak.old rather than deleted, refusing outright if a previous
// .bak.old is still there unresolved; --force skips that and overwrites
// in place.
func repairDependencyDirs(repoRoot, srcRoot, worktreePath string) (string, error) {
	projectCfg, err := config.LoadProjectConfig(repoRoot)
	if err != nil {
		return "", err
	}

	projectType := project.Detect(repoRoot)
	dirs := config.ResolveDependencyDirs(builtinDependencyDirsByType[projectType], projectCfg)
	if len(dirs) == 0 {
		explainNoDependencyDirs(projectType)
		return config.DepsCloneModeNone, nil
	}

	fallback := fallbackUndecided
	worstMode := fs.CloneModeReflink
	any := false

	for _, dir := range dirs {
		src := filepath.Join(srcRoot, dir)
		if _, err := os.Stat(src); os.IsNotExist(err) {
			continue
		}

		dst := filepath.Join(worktreePath, dir)
		mode, skipped, err := repairOneDependencyDir(src, dst, &fallback)
		if err != nil {
			return "", explainCloneFailure(err, worktreePath)
		}
		if skipped {
			continue
		}
		any = true
		if mode > worstMode {
			worstMode = mode
		}
	}

	if !any {
		return "", nil
	}
	reportCloneMode(worstMode)
	return worstMode.String(), nil
}

// repairOneDependencyDir probes whether src can be reflinked before
// touching dst at all, so a run that ends in the user picking [p] (pool
// init) or aborting the prompt never backs dst up in the first place —
// previously, backUpExistingDependencyDir ran unconditionally up front,
// so a Ctrl-C at the h/c/s/p prompt left a real .bak.old on disk that
// blocked every subsequent `grove repair` with "refusing to repair"
// until the user manually removed it, even though nothing had actually
// gone wrong yet. Only once cloneOneDependencyDir is about to write
// (fallback resolved to something other than skip, or the pool-init
// retry lands on a fresh reflink) does dst get backed up.
func repairOneDependencyDir(src, dst string, fallback *fallbackChoice) (fs.CloneMode, bool, error) {
	if _, err := os.Stat(dst); err != nil {
		return cloneOneDependencyDir(src, dst, fallback)
	}

	staging := dst + ".grove-repair-tmp"
	if err := os.RemoveAll(staging); err != nil {
		return 0, false, fmt.Errorf("clear stale repair staging dir %s: %w", staging, err)
	}
	defer os.RemoveAll(staging)

	mode, skipped, err := cloneOneDependencyDir(src, staging, fallback)
	if err != nil || skipped {
		return mode, skipped, err
	}

	if err := backUpExistingDependencyDir(dst); err != nil {
		return 0, false, err
	}
	if err := os.Rename(staging, dst); err != nil {
		return 0, false, err
	}
	return mode, false, nil
}

// backUpExistingDependencyDir moves dst out of the way before repair
// writes a fresh clone in its place. With --force it removes dst outright
// instead, since the whole point of --force is skipping this safety net.
func backUpExistingDependencyDir(dst string) error {
	if repairForce {
		return os.RemoveAll(dst)
	}

	backup := dst + ".bak.old"
	if _, err := os.Stat(backup); err == nil {
		return fmt.Errorf("refusing to repair %s: a previous .bak.old already exists at %s (remove or rename it first)", output.Path(dst), output.Path(backup))
	} else if !os.IsNotExist(err) {
		return err
	}

	if err := os.Rename(dst, backup); err != nil {
		return err
	}
	fmt.Println(output.Dim(fmt.Sprintf("Kept previous copy of %s at %s (delete once verified)", dst, backup)))
	return nil
}
