package fs

import (
	"bufio"
	"errors"
	"io"
	"os"
	"strings"
)

// HealthStatus classifies the result of CheckHealth. A path can pass a
// plain os.Stat while still being unusable — a btrfs subvolume that lost
// its parent, a mount that flipped read-only mid-session.
type HealthStatus int

const (
	Healthy HealthStatus = iota
	Missing
	Unreadable
	ReadOnly
	// NoGitRecord marks a path that passes every filesystem-level check
	// but has no corresponding entry in `git worktree list` — its
	// .git/worktrees/<name> record was removed outside Grove while the
	// directory itself was left behind. CheckHealth can't detect this on
	// its own; callers that cross-reference git.ListWorktrees() (see
	// cmd/status.go's unhealthyCandidates) assign it after the fact.
	NoGitRecord
)

func (s HealthStatus) String() string {
	switch s {
	case Healthy:
		return "healthy"
	case Missing:
		return "missing"
	case Unreadable:
		return "unreadable"
	case ReadOnly:
		return "read-only"
	case NoGitRecord:
		return "no git worktree record"
	default:
		return "unknown"
	}
}

// CheckHealth classifies path's usability with a real read, not just a
// stat: a directory can stat successfully while every syscall against its
// contents fails (EIO from a corrupted btrfs subvolume, ENOTCONN from a
// torn-down mount).
func CheckHealth(path string) HealthStatus {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Missing
		}
		return Unreadable
	}

	f, err := os.Open(path)
	if err != nil {
		return Unreadable
	}
	defer f.Close()

	if _, err := f.Readdirnames(1); err != nil && !errors.Is(err, io.EOF) {
		return Unreadable
	}

	if isReadOnlyMount(path) {
		return ReadOnly
	}
	return Healthy
}

// isReadOnlyMount reports whether the mount covering path is mounted "ro".
// It matches the longest mount point that prefixes path, since a deeper
// bind mount can be read-write even while its parent is read-only (or
// vice versa).
func isReadOnlyMount(path string) bool {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return false
	}
	defer f.Close()

	var bestMatch string
	var bestReadOnly bool

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 4 {
			continue
		}
		mountPoint := fields[1]
		if !strings.HasPrefix(path, mountPoint) {
			continue
		}
		if len(mountPoint) < len(bestMatch) {
			continue
		}
		bestMatch = mountPoint
		bestReadOnly = isReadOnlyOption(fields[3])
	}

	return bestReadOnly
}

func isReadOnlyOption(options string) bool {
	for _, opt := range strings.Split(options, ",") {
		if opt == "ro" {
			return true
		}
	}
	return false
}
