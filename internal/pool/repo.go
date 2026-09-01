package pool

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/dyung/grove/internal/fs"
	"github.com/dyung/grove/internal/output"
)

// ErrUnsafeVolumeLabel is returned when a label meant to become a single
// pool path segment (a repo name or a Grove worktree Name) contains a
// path separator. This should never happen for data Grove itself wrote —
// resolveWorktreeName (cmd/create.go) flattens branch slashes before a
// Name is ever stored, and grove adopt's default Name comes from
// filepath.Base, already a single segment — so seeing this means the
// registry holds an older or hand-edited entry that predates that
// guarantee. Rejecting loudly here, at the one place every volume name is
// built, surfaces that bad data immediately instead of silently
// reshaping it into a different string than what `grove list`/`grove
// repair <name>` actually show the user.
var ErrUnsafeVolumeLabel = fmt.Errorf("worktree or repo name contains a path separator")

func requireSafeVolumeLabel(label string) error {
	if strings.Contains(label, "/") {
		return fmt.Errorf("%w: %q (rename it first, e.g. via `grove rename`)", ErrUnsafeVolumeLabel, label)
	}
	return nil
}

// EnsureDependencyDirVolume makes sure worktreeRoot's dependency dir at
// depRelPath (e.g. "node_modules") is backed by a Btrfs subvolume inside
// the pool, rather than living as a plain directory on worktreeRoot's own
// filesystem. This is what makes that dependency dir a valid reflink
// *source* for other worktrees: reflink can never cross filesystems, so a
// dependency dir sitting on ext4 can only ever be hardlinked or copied
// into the pool, never reflinked from.
//
// worktreeLabel identifies which worktree owns this copy inside the pool
// (e.g. "main", or a branch name), so the main repo's node_modules and a
// feature worktree's node_modules don't collide on the same pool path.
// A pre-existing real directory at depRelPath is migrated into the
// subvolume rather than discarded, exactly like EnsureWorktreeVolume does
// for a worktree's own container directory.
func EnsureDependencyDirVolume(mountPoint, repoName, worktreeLabel, worktreeRoot, depRelPath string) (path, backupPath string, err error) {
	if err := requireSafeVolumeLabel(repoName); err != nil {
		return "", "", err
	}
	if err := requireSafeVolumeLabel(worktreeLabel); err != nil {
		return "", "", err
	}
	volumeName := fmt.Sprintf("%s-%s-%s", repoName, worktreeLabel, filepath.Base(depRelPath))
	sibling := filepath.Join(worktreeRoot, depRelPath)
	subvolume := filepath.Join(mountPoint, volumeName)

	info, err := os.Lstat(sibling)
	switch {
	case os.IsNotExist(err):
		return sibling, "", createSubvolume(subvolume, sibling)
	case err != nil:
		return sibling, "", err
	case info.Mode()&os.ModeSymlink != 0:
		return sibling, "", nil
	default:
		backup, err := migrateIntoSubvolume(subvolume, sibling)
		return sibling, backup, err
	}
}

// EnsureWorktreeVolume makes sure a single already-existing worktree
// directory at worktreePath is itself backed by a Btrfs subvolume inside
// the pool, rather than staying on its own filesystem (e.g. ext4)
// forever. `grove create` calls this right after `git worktree add`;
// `grove repair` calls it as a retrofit for a worktree that never went
// through it — migrating only its *dependency dirs* can't make them
// reflink if the worktree's own container is still on the wrong
// filesystem, since reflink can't cross filesystems.
//
// repoName and worktreeLabel key the subvolume name the same way
// EnsureDependencyDirVolume does, and a pre-existing real directory at
// worktreePath is migrated in rather than discarded, same as there.
func EnsureWorktreeVolume(mountPoint, repoName, worktreeLabel, worktreePath string) (path, backupPath string, err error) {
	if err := requireSafeVolumeLabel(repoName); err != nil {
		return "", "", err
	}
	if err := requireSafeVolumeLabel(worktreeLabel); err != nil {
		return "", "", err
	}
	volumeName := fmt.Sprintf("%s-%s", repoName, worktreeLabel)
	subvolume := filepath.Join(mountPoint, volumeName)

	info, err := os.Lstat(worktreePath)
	switch {
	case os.IsNotExist(err):
		return worktreePath, "", createSubvolume(subvolume, worktreePath)
	case err != nil:
		return worktreePath, "", err
	case info.Mode()&os.ModeSymlink != 0:
		return worktreePath, "", nil
	default:
		backup, err := migrateIntoSubvolume(subvolume, worktreePath)
		return worktreePath, backup, err
	}
}

// SplitRepoVolume converts a repo still on the retired whole-container
// layout (one subvolume backing the entire <repo>.wt, from the retired
// EnsureRepoVolume) into Grove's per-worktree layout: one subvolume per
// tracked worktree, with <repo>.wt itself left as a plain directory.
// No-ops, reporting migrated as false, when <repo>.wt isn't currently a
// symlink into the pool — including repos that were always per-worktree,
// and ones already migrated by an earlier run of this command.
//
// trackedNames identifies which top-level entries in the old container
// are actual Grove worktrees (the registry's Worktree.Name values for
// this repo); each of those gets its own new subvolume. Anything else
// found alongside them (e.g. a Syncthing .stfolder marker, or other
// incidental data that happened to live next to the worktrees) is copied
// back as a plain directory instead — it was never CoW-managed by Grove
// and doesn't need a subvolume of its own.
//
// The original container subvolume is renamed to <repo>.wt.grove-bak
// inside the pool rather than deleted, so a failed or partial migration
// leaves recoverable data; each entry inside it is rsync'd into its
// destination rather than moved, since a subvolume can't be created
// around an existing directory in place. The backup's path is returned
// alongside migrated so the caller can record it in the registry for
// `grove cleanup`/`grove status`.
func SplitRepoVolume(mountPoint, repoRoot string, trackedNames []string) (migrated bool, backupPath string, err error) {
	repoName := filepath.Base(repoRoot)
	container := filepath.Join(filepath.Dir(repoRoot), repoName+".wt")

	info, err := os.Lstat(container)
	if os.IsNotExist(err) {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return false, "", nil
	}

	oldSubvolume, err := filepath.EvalSymlinks(container)
	if err != nil {
		return false, "", err
	}

	entries, err := os.ReadDir(oldSubvolume)
	if err != nil {
		return false, "", err
	}

	if err := ensureCommand("rsync", "rsync", nil); err != nil {
		return false, "", fmt.Errorf("install rsync: %w", err)
	}

	tracked := make(map[string]bool, len(trackedNames))
	for _, name := range trackedNames {
		tracked[name] = true
	}

	if err := os.Remove(container); err != nil {
		return false, "", err
	}
	if err := os.MkdirAll(container, 0o755); err != nil {
		return false, "", err
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		name := entry.Name()
		src := filepath.Join(oldSubvolume, name)
		dst := filepath.Join(container, name)

		if !tracked[name] {
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return false, "", err
			}
			if err := runLocal("rsync", "-a", src+"/", dst+"/"); err != nil {
				return false, "", fmt.Errorf("copy %s into %s: %w", src, dst, err)
			}
			continue
		}

		if err := requireSafeVolumeLabel(name); err != nil {
			return false, "", err
		}

		volumeName := fmt.Sprintf("%s-%s", repoName, name)
		newSubvolume := filepath.Join(mountPoint, volumeName)

		if err := runLocal("btrfs", "subvolume", "create", newSubvolume); err != nil {
			return false, "", err
		}
		if err := runLocal("rsync", "-a", src+"/", newSubvolume+"/"); err != nil {
			return false, "", fmt.Errorf("copy %s into %s: %w", src, newSubvolume, err)
		}
		if err := os.Symlink(newSubvolume, dst); err != nil {
			return false, "", err
		}
	}

	backup := oldSubvolume + ".grove-bak"
	if err := os.Rename(oldSubvolume, backup); err != nil {
		return false, "", err
	}

	fmt.Println(output.Dim(fmt.Sprintf("Migrated %s into per-worktree subvolumes; original container kept at %s (run `grove cleanup` once verified)", output.Path(container), output.Path(backup))))
	return true, backup, nil
}

func createSubvolume(subvolume, sibling string) error {
	if err := runLocal("btrfs", "subvolume", "create", subvolume); err != nil {
		return err
	}
	return os.Symlink(subvolume, sibling)
}

func migrateIntoSubvolume(subvolume, sibling string) (string, error) {
	if err := runLocal("btrfs", "subvolume", "create", subvolume); err != nil {
		return "", err
	}

	if err := ensureCommand("rsync", "rsync", nil); err != nil {
		return "", fmt.Errorf("install rsync: %w", err)
	}

	if err := runLocalRsync(sibling+"/", subvolume+"/"); err != nil {
		return "", fmt.Errorf("copy existing %s into pool: %w", sibling, err)
	}

	backup := sibling + ".grove-bak"
	if err := os.Rename(sibling, backup); err != nil {
		return "", err
	}
	if err := os.Symlink(subvolume, sibling); err != nil {
		return "", err
	}

	fmt.Println(output.Dim(fmt.Sprintf("Migrated existing %s into the pool; original kept at %s (run `grove cleanup` once verified)", output.Path(sibling), output.Path(backup))))
	return backup, nil
}

func runLocal(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// runLocalRsync runs `rsync -a src dst`, same as the plain runLocal
// calls elsewhere in this file, but additionally captures stderr so a
// destination-full failure can be recognized and wrapped in
// fs.ErrNoSpace — the same sentinel internal/fs/clone.go's copy fallback
// uses for the same underlying condition, so callers in cmd/ have one
// error to check regardless of which code path (fs's Go-native copy, or
// pool's shelled-out rsync) actually hit the full pool. Without this,
// migrating a whole worktree container into a full pool (this file) and
// migrating a dependency dir into a full pool (internal/fs) surfaced two
// differently-shaped errors for the identical underlying cause, and only
// one of them was ever recognized well enough to suggest a fix.
//
// rsync's own exit code for this case (code 11, "Error in file IO") also
// covers other, unrelated IO failures, so this matches on stderr text
// rather than trusting the exit code alone: rsync consistently prints
// "No space left on device" (from the underlying ENOSPC write failure)
// regardless of rsync version or protocol, where the exit code by itself
// wouldn't distinguish "disk full" from "permission denied mid-transfer"
// or any other IO error also mapped to 11.
func runLocalRsync(src, dst string) error {
	cmd := exec.Command("rsync", "-a", src, dst)
	cmd.Stdout = os.Stdout

	var stderr bytes.Buffer
	cmd.Stderr = io.MultiWriter(os.Stderr, &stderr)

	err := cmd.Run()
	if err == nil {
		return nil
	}
	if strings.Contains(stderr.String(), "No space left on device") {
		return fmt.Errorf("%w: %s", fs.ErrNoSpace, dst)
	}
	return err
}

// DeleteSubvolume removes the single Btrfs subvolume backing linkPath
// (a pool-resident worktree directory or dependency dir, always a
// symlink into the pool per EnsureWorktreeVolume/
// EnsureDependencyDirVolume) and the now-dangling symlink itself. This
// is the narrow, single-subvolume fix for a corrupted worktree —
// distinct from Recreate, which wipes the entire pool. It only ever
// touches the one subvolume linkPath resolves to; every other worktree
// and dependency dir in the pool is untouched.
//
// linkPath must already resolve to a path inside the pool; the caller
// is responsible for having confirmed that (see requireHealthyWorktree's
// callers in cmd/) and for getting the user's confirmation before
// calling this, since it permanently discards that worktree's contents
// — acceptable only because everything reflinked into the pool is a
// disposable clone of data that still exists in git or in the original
// dependency-dir source.
func DeleteSubvolume(linkPath, mountPoint string) error {
	subvolume, err := filepath.EvalSymlinks(linkPath)
	if err != nil {
		return fmt.Errorf("resolve %s to its pool subvolume: %w", linkPath, err)
	}
	if !strings.HasPrefix(subvolume, mountPoint) {
		return fmt.Errorf("%s resolves to %s, which isn't inside the pool at %s — refusing to delete it as a subvolume", linkPath, subvolume, mountPoint)
	}

	if err := runLocal("btrfs", "subvolume", "delete", subvolume); err != nil {
		return fmt.Errorf("delete subvolume %s: %w", subvolume, err)
	}
	return os.Remove(linkPath)
}
