package cmd

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dyung/grove/internal/config"
	"github.com/dyung/grove/internal/fs"
	"github.com/dyung/grove/internal/git"
	"github.com/dyung/grove/internal/output"
	"github.com/dyung/grove/internal/pool"
	"github.com/dyung/grove/internal/project"
	"github.com/spf13/cobra"
)

// builtinDependencyDirsByType maps a detected project type to the
// dependency directories Grove clones by default for that ecosystem.
// project.Unknown falls back to node_modules, matching npm, so an
// undetected project still gets a reasonable default instead of none.
var builtinDependencyDirsByType = map[project.Type][]string{
	project.Unknown: {"node_modules"},
	project.NPM:     {"node_modules"},
	// PNPM intentionally clones nothing by default: pnpm's node_modules is
	// mostly symlinks into a shared global store already, so there's no
	// meaningful CoW win from cloning it, just a wasted directory walk. A
	// project that needs it cloned anyway can add "node_modules" back via
	// dependency_dirs in .grove.json.
	project.PNPM:   {},
	project.Cargo:  {"target"},
	project.Python: {".venv"},
	// Go intentionally clones nothing by default: the module cache lives
	// outside the repo (GOPATH/pkg/mod, shared across all worktrees
	// already), and there's no vendor dir unless the project explicitly
	// vendors, which dependency_dirs in .grove.json can still opt into.
	project.Go: {},
}

var createCmd = &cobra.Command{
	Use:   "create <branch>",
	Short: "Create a new worktree for a branch",
	Args:  cobra.ExactArgs(1),
	RunE:  runCreate,
}

var createNoDeps bool
var createFrom string
var createName string

func init() {
	createCmd.Flags().BoolVar(&createNoDeps, "no-deps", false, "skip cloning dependency dirs (node_modules, target, .venv, etc.) entirely")
	createCmd.Flags().StringVar(&createFrom, "from", "", "clone dependency dirs from this worktree (name or path) instead of the main repo; migrates it into the pool first if needed so it can actually be reflinked from")
	createCmd.Flags().StringVar(&createName, "name", "", "worktree name and directory (default: the branch name, with slashes flattened to hyphens)")
	rootCmd.AddCommand(createCmd)
}

func runCreate(cmd *cobra.Command, args []string) error {
	branch := args[0]

	repoRoot, err := git.MainRepoRoot()
	if err != nil {
		return err
	}

	reg, err := config.Load(repoRoot)
	if err != nil {
		return err
	}

	name := resolveWorktreeName(branch, createName)
	if _, found := reg.Find(name); found {
		return fmt.Errorf("worktree %q already exists (see `grove list`); pass --name to use a different one", name)
	}

	newBranch, err := ensureBranch(branch)
	if err != nil {
		return err
	}

	worktreesDir, err := resolveWorktreesDir(repoRoot)
	if err != nil {
		return err
	}

	worktreePath := filepath.Join(worktreesDir, name)
	if newBranch {
		err = git.WorktreeAddNewBranch(worktreePath, branch)
	} else {
		err = git.WorktreeAdd(worktreePath, branch)
	}
	if err != nil {
		return err
	}

	if err := ensureWorktreePoolResident(repoRoot, name, worktreePath); err != nil {
		return err
	}

	depsSrcRoot, err := resolveDepsSourceRoot(repoRoot, createFrom)
	if err != nil {
		return err
	}

	depsCloneMode, err := cloneDependencyDirs(repoRoot, depsSrcRoot, worktreePath)
	if err != nil {
		return err
	}

	if err := recordWorktree(repoRoot, name, branch, worktreePath, depsCloneMode); err != nil {
		return err
	}

	fmt.Println(describeCreated(name, branch, worktreePath))
	return nil
}

// describeCreated reports the worktree's name, branch, and path,
// always all three — see describeListEntry in list.go for why Name and
// Branch are shown even when they're the same string right now.
func describeCreated(name, branch, worktreePath string) string {
	return fmt.Sprintf("%s Created worktree %s (branch %s) at %s", output.Success("✓"), output.Name(name), branch, output.Path(worktreePath))
}

// resolveWorktreeName picks the stable identifier Grove uses for this
// worktree in the registry and as its directory name under <repo>.wt/.
// An explicit --name always wins. Otherwise it derives one from branch,
// flattening slashes to hyphens (feat/drive-picker -> feat-drive-picker)
// so the name stays a single path segment — lookups like --from <name>
// and `grove repair <name>` treat it as one atomic token, and a literal
// nested directory could collide with git worktree add. Branch itself is
// left untouched: Name and Branch are intentionally decoupled from here
// on, since `grove rename` only changes Name while a plain `git checkout
// -b` inside the worktree changes Branch independently.
func resolveWorktreeName(branch, explicit string) string {
	if explicit != "" {
		return explicit
	}
	return strings.ReplaceAll(branch, "/", "-")
}

// ensureBranch checks whether branch already exists locally. If not, it
// asks the user whether to create it as a new branch; declining aborts
// rather than letting git fail deeper into the command with a confusing
// "invalid reference" error. Returns whether the branch needs to be
// created (true) or already existed (false).
func ensureBranch(branch string) (bool, error) {
	exists, err := git.BranchExists(branch)
	if err != nil {
		return false, err
	}
	if exists {
		return false, nil
	}

	fmt.Printf("Branch %s doesn't exist yet.\n", output.Name(branch))
	fmt.Print("Create it? [Y/n] ")

	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil {
		return false, err
	}

	answer := strings.ToLower(strings.TrimSpace(line))
	if answer == "n" || answer == "no" {
		return false, fmt.Errorf("aborted: branch %q does not exist", branch)
	}
	return true, nil
}

// resolveWorktreesDir picks where new worktrees for repoRoot should live.
// It always returns the plain sibling .wt directory for `git worktree
// add` to write into normally — pool residency, when needed, is applied
// afterward to the specific new worktree path via
// ensureWorktreePoolResident, not pre-provisioned here for the whole
// container. When the filesystem can't reflink natively and the pool
// isn't mounted yet, the user is asked to confirm falling back to a
// plain (non-CoW) worktree rather than choosing that on their behalf.
func resolveWorktreesDir(repoRoot string) (string, error) {
	plainDir := filepath.Join(repoRoot, "..", filepath.Base(repoRoot)+".wt")

	if fs.DetectCloneSupport(filepath.Dir(repoRoot)) == fs.CloneSupported {
		return plainDir, nil
	}

	paths, err := pool.DefaultPaths()
	if err != nil {
		return "", err
	}

	mounted, err := pool.IsMounted(paths.MountPoint)
	if err != nil {
		return "", err
	}
	if !mounted {
		proceed, err := confirmPlainFallback()
		if err != nil {
			return "", err
		}
		if !proceed {
			return "", fmt.Errorf("aborted: run %s first, then re-run `grove create`", output.Command("grove pool init"))
		}
	}

	return plainDir, nil
}

// confirmPlainFallback asks the user whether to proceed with a plain,
// non-CoW worktree rather than silently choosing that on their behalf.
func confirmPlainFallback() (bool, error) {
	fmt.Println(output.Warn("This filesystem has no reflink support and the Grove pool isn't set up."))
	fmt.Println("Run " + output.Command("grove pool init") + " first for fast, disk-efficient CoW clones.")
	fmt.Print("Continue anyway with a plain (non-CoW) worktree? [y/N] ")

	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil {
		return false, err
	}

	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes", nil
}

// cloneDependencyDirs clones each resolved dependency dir from srcRoot
// into worktreePath, and reports the worst CloneMode used across all of
// them as a string suitable for config.Worktree.DepsCloneMode ("" if
// --no-deps was set or no dependency dirs existed to clone). The
// dependency-dir *list* is always resolved from repoRoot's .grove.json
// (that config is a property of the project, not of any one worktree),
// but the actual files cloned come from srcRoot, which callers may point
// at a worktree other than the main repo via --from.
func cloneDependencyDirs(repoRoot, srcRoot, worktreePath string) (string, error) {
	if createNoDeps {
		fmt.Println(output.Dim("Skipping dependency dirs: --no-deps was set."))
		return "", nil
	}

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

	// fallback remembers the user's answer the first time a reflink attempt
	// fails, so a project with several dependency dirs (e.g. node_modules
	// and a nested workspace package's node_modules) only prompts once per
	// `grove create` run rather than once per dir.
	fallback := fallbackUndecided
	worstMode := fs.CloneModeReflink
	any := false

	for _, dir := range dirs {
		src := filepath.Join(srcRoot, dir)
		if _, err := os.Stat(src); os.IsNotExist(err) {
			continue
		}

		dst := filepath.Join(worktreePath, dir)
		mode, skipped, err := cloneOneDependencyDir(src, dst, &fallback)
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

// resolveDepsSourceRoot turns the --from flag's value into an absolute
// path to clone dependency dirs from. Empty (the default) means the main
// repo itself, which `grove pool init` already made pool-resident if the
// pool is set up. A non-empty value is tried first as a tracked worktree
// name (the common case: `--from feature-x`), falling back to treating
// it as a literal filesystem path so `--from /some/other/checkout` also
// works for worktrees Grove doesn't track.
//
// When from names a worktree that isn't already pool-resident, its
// dependency dirs are migrated into the pool first (mirroring what
// `grove pool init` does for the main repo), so cloning from it can
// actually reflink instead of silently falling back to hardlink/copy the
// same way a plain ext4 source always would. If the pool isn't mounted,
// this is skipped and the caller gets the plain path as before — the
// existing hardlink/copy fallback prompt still applies in that case.
func resolveDepsSourceRoot(repoRoot, from string) (string, error) {
	if from == "" {
		return repoRoot, nil
	}
	if from == "main" {
		return "", fmt.Errorf("--from %q is reserved for the main repo itself; omit --from to use it", from)
	}

	reg, err := config.Load(repoRoot)
	if err != nil {
		return "", err
	}

	srcRoot := from
	if wt, found := reg.Find(from); found {
		srcRoot = wt.Path
	} else if _, err := os.Stat(from); err != nil {
		return "", fmt.Errorf("--from %q is neither a tracked worktree name (see `grove list`) nor an existing path: %w", from, err)
	}

	if err := ensureDepsSourcePoolResident(repoRoot, from, srcRoot); err != nil {
		return "", err
	}
	return srcRoot, nil
}

// ensureDepsSourcePoolResident migrates srcRoot's dependency dirs into
// the pool if they aren't there already, so a --from worktree that was
// never reflinked itself (e.g. adopted from a pre-Grove layout, or
// created before the pool existed) can still serve as a real reflink
// source instead of forcing every clone from it into hardlink/copy.
// No-ops if the pool isn't mounted, leaving today's plain-path behavior
// intact.
func ensureDepsSourcePoolResident(repoRoot, worktreeLabel, srcRoot string) error {
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

	dirs, err := resolveRepoDependencyDirs(repoRoot)
	if err != nil {
		return err
	}

	repoName := filepath.Base(repoRoot)
	for _, dir := range dirs {
		if _, err := os.Stat(filepath.Join(srcRoot, dir)); os.IsNotExist(err) {
			continue
		}

		if _, err := pool.EnsureDependencyDirVolume(paths.MountPoint, repoName, worktreeLabel, srcRoot, dir); err != nil {
			return fmt.Errorf("move %s into the pool: %w", dir, err)
		}
	}
	return nil
}

// fallbackChoice is the user's answer, made at most once per `grove
// create` run, for what to do when a dependency dir can't be reflinked.
type fallbackChoice int

const (
	fallbackUndecided fallbackChoice = iota
	fallbackHardlink
	fallbackCopy
	fallbackSkip
	fallbackPoolInit
)

// cloneOneDependencyDir attempts a reflink clone of src into dst. If that
// fails, it consults *fallback: prompting the user the first time (and
// caching the answer in *fallback for subsequent dirs this run), then
// applying whatever was chosen. skipped reports whether the dir was left
// uncloned, either by the user's choice or because reflink outright
// succeeded and no fallback was needed.
func cloneOneDependencyDir(src, dst string, fallback *fallbackChoice) (mode fs.CloneMode, skipped bool, err error) {
	_, err = fs.CloneTreeReflinkOnly(src, dst)
	if err == nil {
		return fs.CloneModeReflink, false, nil
	}
	if !errors.Is(err, fs.ErrCloneUnsupported) {
		// A real error (permissions, no space, etc.), not just "this
		// filesystem can't reflink" — nothing to prompt about.
		return 0, false, err
	}

	// A prior attempt against this same dst (e.g. repair.go's [p] retry)
	// may have left partial output — clear it so every attempt below starts
	// clean, reflink retry included.
	if err := os.RemoveAll(dst); err != nil {
		return 0, false, fmt.Errorf("clear partial reflink output at %s: %w", dst, err)
	}

	if *fallback == fallbackUndecided {
		*fallback, err = promptCloneFallback(src)
		if err != nil {
			return 0, false, err
		}
	}

	if *fallback == fallbackPoolInit {
		if err := runPoolInit(nil, nil); err != nil {
			return 0, false, fmt.Errorf("grove pool init: %w", err)
		}
		// Only useful once: after the pool exists, later dirs in this run go
		// straight back through a fresh reflink attempt rather than assuming
		// pool init again, which would just no-op loudly every time.
		*fallback = fallbackUndecided
		return cloneOneDependencyDir(src, dst, fallback)
	}

	switch *fallback {
	case fallbackSkip:
		return 0, true, nil
	case fallbackHardlink:
		mode, err := fs.CloneTreeHardlinkOrCopy(src, dst, true)
		return mode, false, err
	default: // fallbackCopy
		mode, err := fs.CloneTreeHardlinkOrCopy(src, dst, false)
		return mode, false, err
	}
}

// promptCloneFallback asks the user, once per `grove create` run, how to
// handle dependency dirs when this filesystem can't reflink them —
// mirroring confirmPlainFallback's pattern of asking before silently
// choosing a less efficient or more resource-hungry strategy on the
// user's behalf.
//
// The reason reflink failed for src matters for what to tell the user:
// if the pool isn't mounted at all, there's genuinely nothing more to do
// here besides hardlink/copy or set the pool up. But if the pool *is*
// mounted and src just isn't inside it yet, saying "this filesystem can't
// reflink" is misleading — there's a real fix (`grove pool init`, or
// --from) the user hasn't been told about.
func promptCloneFallback(src string) (fallbackChoice, error) {
	fmt.Println(reflinkUnavailableMessage(src))

	// Ordered by how much a typical user should actually reach for each
	// option: pool init (the real, permanent fix) first when it's on the
	// table, then copy (safe, if wasteful), and only then hardlink —
	// hardlink is the riskiest choice since editing files inside a
	// dependency dir from *either* worktree corrupts the other, something
	// most users doing everyday work don't expect and don't want, so it's
	// listed last rather than as the default-looking first option.
	offered := "c/h/s"
	if poolInitOffersFix(src) {
		fmt.Println("  " + output.Bold("[p]") + " " + output.Commandf("grove pool init") + " — set up the pool now, then retry this dir automatically")
		offered = "p/" + offered
	}
	fmt.Println("  " + output.Bold("[c]") + " Copy       — full duplicate, safe to edit, uses more disk space")
	fmt.Println("  " + output.Bold("[h]") + " Hardlink   — no extra disk space, but don't edit files inside from either worktree")
	fmt.Println("  " + output.Bold("[s]") + " Skip       — don't clone dependency dirs at all; you'll need to install them yourself")
	fmt.Printf("Choose [%s]: ", offered)

	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil {
		return fallbackUndecided, err
	}

	switch strings.ToLower(strings.TrimSpace(line)) {
	case "h", "hardlink":
		return fallbackHardlink, nil
	case "s", "skip":
		return fallbackSkip, nil
	case "p", "pool", "pool init":
		if !poolInitOffersFix(src) {
			return fallbackCopy, nil
		}
		return fallbackPoolInit, nil
	default:
		return fallbackCopy, nil
	}
}

// poolInitOffersFix reports whether running `grove pool init` right now
// would actually change the outcome for src — true when either no pool
// exists yet, or one exists but src hasn't been migrated into it, both
// of which `grove pool init` addresses. False when src is already
// pool-resident and reflink still failed, a genuine filesystem
// limitation pool init can't fix, so offering it there would be a dead
// end disguised as a fix.
func poolInitOffersFix(src string) bool {
	paths, err := pool.DefaultPaths()
	if err != nil {
		return false
	}

	mounted, err := pool.IsMounted(paths.MountPoint)
	if err != nil {
		return false
	}
	if !mounted {
		return true
	}
	return !isPoolResident(src, paths.MountPoint)
}

// isPoolResident reports whether src currently resolves into the pool.
// A migrated dependency dir is a symlink (see EnsureDependencyDirVolume)
// whose own path is unchanged — only its target moves into the pool — so
// checking src's literal string against paths.MountPoint never matches
// even right after a successful migration, which is what made the [p]
// retry loop forever: migration kept succeeding, but this check kept
// reporting it hadn't. Resolving through the symlink first is what
// actually reflects where the data lives. If src doesn't exist yet (or
// isn't a symlink), EvalSymlinks just returns src unchanged, preserving
// the original plain-path behavior.
func isPoolResident(src, mountPoint string) bool {
	resolved, err := filepath.EvalSymlinks(src)
	if err != nil {
		resolved = src
	}
	return strings.HasPrefix(resolved, mountPoint)
}

// reflinkUnavailableMessage explains why src couldn't be reflinked,
// distinguishing "no pool exists, this is a dead end without one" from
// "the pool exists, but src specifically hasn't been migrated into it" —
// the latter has a real fix (`grove pool init`, or supplying --from
// correctly) that the generic message would otherwise hide.
func reflinkUnavailableMessage(src string) string {
	paths, err := pool.DefaultPaths()
	if err != nil {
		return output.Warn("This filesystem can't reflink-clone dependency dirs here.")
	}

	mounted, err := pool.IsMounted(paths.MountPoint)
	if err != nil || !mounted {
		return output.Warn("This filesystem can't reflink-clone dependency dirs here (no CoW support, and the Grove pool isn't set up).")
	}

	if isPoolResident(src, paths.MountPoint) {
		// src is already inside the pool but still failed to reflink — a
		// genuine cross-device or filesystem-limitation case, not a missing
		// migration.
		return output.Warn("This filesystem can't reflink-clone dependency dirs here.")
	}

	return fmt.Sprintf("%s %s isn't inside the Grove pool yet, so it can't be reflinked from. Run %s again from the main repo to migrate its dependency dirs into the pool, or re-run this with %s to migrate that worktree's instead.",
		output.Warn("Note:"), output.Path(src), output.Command("grove pool init"), output.Command("--from <worktree>"))
}

// explainNoDependencyDirs tells the user why a project type resolved to
// zero dependency dirs to clone, rather than silently doing nothing —
// the reasoning differs by ecosystem (pnpm's node_modules is symlinks
// into a shared store, Go's module cache lives outside the repo
// entirely), and without an explanation it looks identical to Grove
// having failed to detect the project at all.
func explainNoDependencyDirs(projectType project.Type) {
	var reason string
	switch projectType {
	case project.PNPM:
		reason = "pnpm's node_modules is mostly symlinks into a shared global store, so there's nothing worth reflinking"
	case project.Go:
		reason = "Go's module cache lives outside the repo (GOPATH/pkg/mod), shared across worktrees already"
	default:
		fmt.Println(output.Dim("No dependency dirs to clone for this project."))
		return
	}
	fmt.Println(output.Dim(fmt.Sprintf("Skipping dependency dirs for %s: %s.", projectType, reason)))
	fmt.Println(output.Dim("Add dependency_dirs in .grove.json to opt back in."))
}

// reportCloneMode tells the user which fallback, if any, Grove had to use
// for dependency dirs, since silently downgrading from CoW to a hardlink
// or full copy would otherwise be invisible until something surprising
// happened later (e.g. unexpectedly high disk usage from a full copy).
func reportCloneMode(mode fs.CloneMode) {
	switch mode {
	case fs.CloneModeReflink:
		// Common case; no need to narrate success.
	case fs.CloneModeHardlink:
		fmt.Println(output.Warn("Note: dependency dirs were hardlinked (no extra disk usage, but don't edit files inside them from either worktree)."))
	case fs.CloneModeCopy:
		fmt.Println(output.Warn("Note: dependency dirs were fully copied (uses extra disk space)."))
	}
}

// explainCloneFailure turns a bare out-of-space error into guidance the
// user can act on: pool-growth commands if the failure happened inside
// the Grove pool, general disk-space advice otherwise.
func explainCloneFailure(err error, worktreePath string) error {
	if !errors.Is(err, fs.ErrNoSpace) {
		return fmt.Errorf("clone dependency dir: %w", err)
	}

	paths, poolErr := pool.DefaultPaths()
	if poolErr == nil && strings.HasPrefix(worktreePath, paths.MountPoint) {
		return fmt.Errorf("%w\nthe Grove pool is full; grow it with:\n  %s\n  %s\n(pick N for however much more space you need)",
			err,
			output.Commandf("truncate -s +N %s", paths.Image),
			output.Commandf("sudo btrfs filesystem resize +N %s", paths.MountPoint))
	}

	return fmt.Errorf("%w\nout of disk space at %s; free up space and try again", err, output.Path(worktreePath))
}

func recordWorktree(repoRoot, name, branch, worktreePath, depsCloneMode string) error {
	reg, err := config.Load(repoRoot)
	if err != nil {
		return err
	}

	reg.Add(config.Worktree{
		Name:          name,
		Path:          worktreePath,
		Branch:        branch,
		CreatedAt:     time.Now(),
		DepsCloneMode: depsCloneMode,
	})

	return reg.Save(repoRoot)
}
