package fs

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

// ErrNoSpace is returned (wrapped) by CloneTreeHardlinkOrCopy when a copy
// fails because the destination filesystem ran out of space, so callers
// can give actionable guidance instead of a bare I/O error.
var ErrNoSpace = errors.New("not enough space to complete clone")

// ErrCloneUnsupported is returned by CloneTreeReflinkOnly when any file in
// the tree couldn't be reflinked — either because the destination
// filesystem doesn't support CoW at all, or because src and dst are on
// different devices. Callers can use this to detect "reflink isn't going
// to work here" and decide how to proceed (e.g. ask the user) rather than
// silently falling back.
var ErrCloneUnsupported = errors.New("reflink clone not supported for this destination")

type CloneSupport int

const (
	CloneUnknown CloneSupport = iota
	CloneSupported
	CloneUnsupported
)

// DetectCloneSupport probes whether dir's filesystem supports reflink
// (copy-on-write) clones, by attempting a real FICLONE ioctl.
func DetectCloneSupport(dir string) CloneSupport {
	src, err := os.CreateTemp(dir, ".grove-clone-probe-*")
	if err != nil {
		return CloneUnknown
	}
	defer os.Remove(src.Name())
	defer src.Close()

	if _, err := src.WriteString("grove"); err != nil {
		return CloneUnknown
	}

	dstPath := src.Name() + ".dst"
	dst, err := os.Create(dstPath)
	if err != nil {
		return CloneUnknown
	}
	defer os.Remove(dstPath)
	defer dst.Close()

	if err := unix.IoctlFileClone(int(dst.Fd()), int(src.Fd())); err != nil {
		return CloneUnsupported
	}
	return CloneSupported
}

// CloneMode records which strategy was actually used to place a regular
// file's data in the destination tree, so callers can tell the user what
// they got instead of leaving it invisible.
type CloneMode int

const (
	// CloneModeReflink means every regular file was placed via a
	// copy-on-write reflink clone (FICLONE).
	CloneModeReflink CloneMode = iota
	// CloneModeHardlink means at least one regular file fell back to a
	// hardlink (same filesystem, no CoW, but no data duplication either).
	CloneModeHardlink
	// CloneModeCopy means at least one regular file fell all the way back
	// to a full byte-for-byte copy (cross-device, or hardlink also failed).
	CloneModeCopy
)

func (m CloneMode) String() string {
	switch m {
	case CloneModeReflink:
		return "reflink"
	case CloneModeHardlink:
		return "hardlink"
	case CloneModeCopy:
		return "copy"
	default:
		return "unknown"
	}
}

// worse reports whether b is a less efficient fallback than a, so
// CloneTreeHardlinkOrCopy can track the single worst mode used across the
// whole walk without the caller needing per-file detail.
func worse(a, b CloneMode) bool { return b > a }

// CloneTreeReflinkOnly recursively copies src into dst using only reflink
// clones (copy-on-write, no data duplication) for regular files — no
// hardlink or full-copy fallback. This is deliberately all-or-nothing: it
// exists so a caller can *test* whether a destination supports reflinking
// before committing to a strategy, rather than silently downgrading
// partway through a large tree.
//
// If any regular file can't be reflinked, the walk stops immediately and
// returns an error wrapping ErrCloneUnsupported. Any partial output
// already written to dst is left in place for the caller to clean up or
// hand off to CloneTreeHardlinkOrCopy — this function does not attempt
// its own rollback, since the caller (cmd/create.go) always follows a
// failed reflink-only attempt with one of those two paths over the same
// dst, which will overwrite or recreate whatever's there.
func CloneTreeReflinkOnly(src, dst string) (bool, error) {
	realSrc, err := resolveCloneRoot(src)
	if err != nil {
		return false, err
	}

	reflinked := true
	err = filepath.Walk(realSrc, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(realSrc, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)

		switch {
		case info.IsDir():
			return os.MkdirAll(target, info.Mode())
		case info.Mode()&os.ModeSymlink != 0:
			return cloneSymlink(path, target)
		default:
			if err := reflinkFile(path, target, info.Mode()); err != nil {
				reflinked = false
				return fmt.Errorf("%w: %s", ErrCloneUnsupported, target)
			}
			return nil
		}
	})
	return reflinked, err
}

// CloneTreeHardlinkOrCopy recursively copies src into dst without
// attempting reflink at all, for use once a caller has already
// established (via CloneTreeReflinkOnly) that reflinking isn't available
// and asked the user how to proceed. When hardlinkOnly is true, each
// regular file is hardlinked (os.Link) if possible, falling back to a
// full byte copy only if hardlinking itself fails (e.g. cross-device).
// When hardlinkOnly is false, every regular file is fully copied,
// skipping the hardlink attempt entirely — this is the user's "Copy"
// choice, and should not silently share inodes even where a hardlink
// would have been possible. Symlinks are recreated rather than followed.
// The returned CloneMode is the least efficient mode used by any file in
// the tree, so callers can tell the user what they actually got.
func CloneTreeHardlinkOrCopy(src, dst string, hardlinkOnly bool) (CloneMode, error) {
	realSrc, err := resolveCloneRoot(src)
	if err != nil {
		return CloneModeCopy, err
	}

	mode := CloneModeHardlink
	err = filepath.Walk(realSrc, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(realSrc, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)

		switch {
		case info.IsDir():
			return os.MkdirAll(target, info.Mode())
		case info.Mode()&os.ModeSymlink != 0:
			return cloneSymlink(path, target)
		default:
			fileMode, err := hardlinkOrCopyFile(path, target, info.Mode(), hardlinkOnly)
			if err != nil {
				return err
			}
			if worse(mode, fileMode) {
				mode = fileMode
			}
			return nil
		}
	})
	return mode, err
}

// resolveCloneRoot follows src past a symlink to its real backing
// directory before either clone function starts walking it. This
// matters because a dependency dir that's been migrated into the Grove
// pool (see pool.EnsureDependencyDirVolume) is always a symlink at the
// worktree/repo level — and filepath.Walk never descends into a symlink
// root, it just visits it once as a single leaf. Without this, cloning
// "from" a pool-resident dir would silently degrade into recreating a
// symlink to the exact same target rather than actually cloning any
// files, leaving the caller with no independent copy at all (and, worse,
// two paths mutating the same underlying data). Symlinks found *within*
// the tree (e.g. npm's node_modules/.bin/* entries) are unaffected —
// they're still visited and recreated as symlinks via cloneSymlink,
// exactly as before.
func resolveCloneRoot(src string) (string, error) {
	resolved, err := filepath.EvalSymlinks(src)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", src, err)
	}
	return resolved, nil
}

func cloneSymlink(src, dst string) error {
	link, err := os.Readlink(src)
	if err != nil {
		return err
	}
	return os.Symlink(link, dst)
}

// reflinkFile places src's data at dst using only a reflink clone
// (copy-on-write, no data duplication). It does not fall back to
// anything else — CloneTreeReflinkOnly relies on that to detect
// unsupported destinations on the very first file rather than after
// walking deep into a large tree.
func reflinkFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	cloneErr := unix.IoctlFileClone(int(out.Fd()), int(in.Fd()))
	out.Close()
	if cloneErr == nil {
		return nil
	}

	// The attempt above may have left a truncated empty file at dst;
	// remove it so the destination isn't left with a corrupt partial file
	// in place of the real fallback CloneTreeHardlinkOrCopy will write.
	if rmErr := os.Remove(dst); rmErr != nil {
		return fmt.Errorf("remove failed reflink target %s: %w", dst, rmErr)
	}
	return cloneErr
}

// hardlinkOrCopyFile places src's data at dst without ever attempting a
// reflink. If hardlinkOnly is true, it tries os.Link first (same
// filesystem, no data duplication, but no CoW: a write through either
// path mutates both, so this is only safe for directories nothing writes
// into post-clone, like dependency trees) and falls back to a full byte
// copy only if the hardlink itself fails (e.g. src and dst are on
// different devices). If hardlinkOnly is false, it copies unconditionally
// — the user asked for real, independent files, not shared inodes.
func hardlinkOrCopyFile(src, dst string, mode os.FileMode, hardlinkOnly bool) (CloneMode, error) {
	if hardlinkOnly {
		if err := os.Link(src, dst); err == nil {
			return CloneModeHardlink, nil
		}
	}

	in, err := os.Open(src)
	if err != nil {
		return CloneModeCopy, err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return CloneModeCopy, err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		if errors.Is(err, syscall.ENOSPC) {
			return CloneModeCopy, fmt.Errorf("%w: %s", ErrNoSpace, dst)
		}
		return CloneModeCopy, fmt.Errorf("copy %s to %s: %w", src, dst, err)
	}
	return CloneModeCopy, nil
}

