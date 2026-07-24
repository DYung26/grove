package git

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// ErrNotAGitRepo indicates the current directory isn't inside a git
// working tree (or any of its parents) — the case a plain git error
// would otherwise report as a raw "exit status 128" with git's own
// flags echoed back, rather than something Grove's callers can act on.
var ErrNotAGitRepo = errors.New("not a git repository (or any of the parent directories)")

func RepoRoot() (string, error) {
	out, err := run("rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// MainRepoRoot returns the working tree root of the main repository, even
// when called from inside a linked worktree. RepoRoot alone isn't enough
// for that case: --show-toplevel from within a worktree returns the
// worktree's own root, not the main repo's, which would make Grove nest
// worktree directories inside each other when `grove create` is run from
// inside an existing worktree instead of the main repo.
func MainRepoRoot() (string, error) {
	out, err := run("rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	commonDir := strings.TrimSpace(out)
	// --git-common-dir points at the main repo's .git directory; its
	// parent is the main repo's working tree root.
	return filepath.Dir(commonDir), nil
}

// BranchExists reports whether branch is a known local branch in the
// current repo.
func BranchExists(branch string) (bool, error) {
	_, err := run("show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	if err == nil {
		return true, nil
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		// show-ref exits 1 for "not found" — that's a real answer, not
		// a failure to determine one.
		return false, nil
	}
	return false, err
}

func WorktreeAdd(path, branch string) error {
	_, err := run("worktree", "add", path, branch)
	return err
}

// WorktreeAddNewBranch creates path as a new worktree on a newly created
// branch, rather than checking out an existing one.
func WorktreeAddNewBranch(path, branch string) error {
	_, err := run("worktree", "add", "-b", branch, path)
	return err
}

// WorktreeRemove removes the worktree at path via `git worktree remove`.
// force allows removal even if the worktree has uncommitted changes.
func WorktreeRemove(path string, force bool) error {
	args := []string{"worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, path)
	_, err := run(args...)
	return err
}

// WorktreePrune runs `git worktree prune`, git's own cleanup for
// worktree administrative state (the entry under .git/worktrees/) whose
// directory no longer exists on disk at all. This is the correct
// git-side step after something outside git's knowledge removed a
// worktree's directory directly — e.g. pool.DeleteSubvolume deleting a
// corrupted worktree's backing subvolume — since WorktreeRemove itself
// requires a live, readable directory to validate against and can't be
// used for a directory that's already gone.
func WorktreePrune() error {
	_, err := run("worktree", "prune")
	return err
}

// WorktreeMove relocates a worktree's directory via `git worktree move`,
// updating git's own worktree administrative files so the branch stays
// correctly linked at its new path.
func WorktreeMove(oldPath, newPath string) error {
	_, err := run("worktree", "move", oldPath, newPath)
	return err
}

// WorktreeInfo describes one entry from `git worktree list`, as needed by
// `grove adopt` to verify a path is a real, already-registered git
// worktree before Grove starts tracking it.
type WorktreeInfo struct {
	Path   string
	Branch string
}

// FindWorktree looks up path (matched after resolving both sides to an
// absolute path, since the user may pass a relative path or one with a
// trailing slash) among the worktrees `git worktree list` already knows
// about for the current repo. Returns found=false, rather than an error,
// when path is a real directory but not a git worktree at all — that's
// an expected outcome for `grove adopt` to handle, not a failure.
func FindWorktree(path string) (info WorktreeInfo, found bool, err error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return WorktreeInfo{}, false, err
	}

	worktrees, err := ListWorktrees()
	if err != nil {
		return WorktreeInfo{}, false, err
	}

	for _, wt := range worktrees {
		if wt.Path == absPath {
			return wt, true, nil
		}
	}
	return WorktreeInfo{}, false, nil
}

// ListWorktrees parses `git worktree list --porcelain` output into
// WorktreeInfo entries, one per worktree of the current repo — including
// the main working tree itself as the first entry. The porcelain format
// is a blank-line-separated series of records, each starting with a
// "worktree <path>" line, optionally followed by "branch <ref>" (bare or
// detached entries omit it, which ListWorktrees represents as an empty
// Branch rather than erroring, since those just aren't adoptable by
// branch-based lookup).
func ListWorktrees() ([]WorktreeInfo, error) {
	out, err := run("worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}

	var worktrees []WorktreeInfo
	var current *WorktreeInfo
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			if current != nil {
				worktrees = append(worktrees, *current)
			}
			current = &WorktreeInfo{Path: strings.TrimPrefix(line, "worktree ")}
		case strings.HasPrefix(line, "branch ") && current != nil:
			current.Branch = strings.TrimPrefix(strings.TrimPrefix(line, "branch "), "refs/heads/")
		}
	}
	if current != nil {
		worktrees = append(worktrees, *current)
	}
	return worktrees, nil
}

func run(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if strings.Contains(stderr.String(), "not a git repository") {
			return "", ErrNotAGitRepo
		}
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, stderr.String())
	}
	return stdout.String(), nil
}
