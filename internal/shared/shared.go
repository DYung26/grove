package shared

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// ErrDestinationExists indicates that a worktree already has its own copy of a shared path.
var ErrDestinationExists = errors.New("shared path destination already exists")

var defaultPatterns = []string{"docs/_local"}
var skippedEnvDirs = map[string]bool{".git": true, ".grove": true, "node_modules": true, "target": true, ".venv": true, "venv": true, "vendor": true, "dist": true, "build": true, ".next": true, ".turbo": true, ".cache": true, "coverage": true, ".pytest_cache": true, ".mypy_cache": true, ".tox": true, "__pycache__": true}
var exampleEnvSuffixes = []string{".example", ".sample", ".template", ".dist"}

// Resolve returns existing, untracked shared paths relative to repoRoot.
func Resolve(repoRoot string, configured, excluded []string) ([]string, error) {
	matches := make(map[string]bool)
	for _, pattern := range append(defaultPatterns, configured...) {
		if err := validatePath(pattern); err != nil {
			return nil, err
		}
		paths, err := filepath.Glob(filepath.Join(repoRoot, filepath.FromSlash(pattern)))
		if err != nil {
			return nil, fmt.Errorf("match shared path %q: %w", pattern, err)
		}
		for _, path := range paths {
			info, err := os.Stat(path)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return nil, err
			}
			if !info.Mode().IsRegular() && !info.IsDir() {
				continue
			}
			rel, err := filepath.Rel(repoRoot, path)
			if err != nil {
				return nil, err
			}
			rel = filepath.ToSlash(rel)
			if isExampleEnvFile(rel) || matchesAny(rel, excluded) || isTracked(repoRoot, rel) {
				continue
			}
			matches[rel] = true
		}
	}
	if err := filepath.WalkDir(repoRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != repoRoot && (path == filepath.Join(repoRoot, "docs", "_local") || skippedEnvDirs[entry.Name()]) {
				return filepath.SkipDir
			}
			return nil
		}
		name := entry.Name()
		if name != ".env" && name != ".envrc" && !strings.HasPrefix(name, ".env.") {
			return nil
		}
		info, err := os.Stat(path)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() && !info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(repoRoot, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if !isExampleEnvFile(rel) && !matchesAny(rel, excluded) && !isTracked(repoRoot, rel) {
			matches[rel] = true
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("scan local environment files: %w", err)
	}
	paths := make([]string, 0, len(matches))
	for path := range matches {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths, nil
}

// Link creates a symlink from worktreeRoot to the canonical path in repoRoot.
// It preserves an existing destination when replace is false and backs it up when true.
func Link(repoRoot, worktreeRoot, rel string, replace bool) (string, error) {
	if err := validatePath(rel); err != nil {
		return "", err
	}
	source := filepath.Join(repoRoot, filepath.FromSlash(rel))
	destination := filepath.Join(worktreeRoot, filepath.FromSlash(rel))
	info, err := os.Stat(source)
	if err != nil {
		return "", fmt.Errorf("shared source %s: %w", source, err)
	}
	if !info.Mode().IsRegular() && !info.IsDir() {
		return "", fmt.Errorf("shared source %s is not a regular file or directory", source)
	}
	if err := ensureParentDirectories(worktreeRoot, filepath.Dir(destination)); err != nil {
		return "", err
	}
	if info, err := os.Lstat(destination); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			resolved, resolveErr := filepath.EvalSymlinks(destination)
			canonical, canonicalErr := filepath.EvalSymlinks(source)
			if resolveErr == nil && canonicalErr == nil && resolved == canonical {
				return "", nil
			}
		}
		if !replace {
			return "", fmt.Errorf("%w: %s", ErrDestinationExists, destination)
		}
		backup := destination + ".grove-bak"
		if _, backupErr := os.Lstat(backup); backupErr == nil {
			return "", fmt.Errorf("refusing to replace %s: backup already exists at %s", destination, backup)
		} else if !os.IsNotExist(backupErr) {
			return "", backupErr
		}
		if err := os.Rename(destination, backup); err != nil {
			return "", err
		}
		if err := os.Symlink(source, destination); err != nil {
			if restoreErr := os.Rename(backup, destination); restoreErr != nil {
				return backup, fmt.Errorf("create shared symlink: %w (restore original from %s: %v)", err, backup, restoreErr)
			}
			return "", err
		}
		return backup, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if err := os.Symlink(source, destination); err != nil {
		return "", err
	}
	return "", nil
}

func validatePath(path string) error {
	clean := filepath.Clean(filepath.FromSlash(path))
	if path == "" || filepath.IsAbs(clean) || clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("shared path %q must be a non-empty path inside the repository", path)
	}
	for _, segment := range strings.Split(filepath.ToSlash(path), "/") {
		if segment == ".." || segment == "." {
			return fmt.Errorf("shared path %q must not contain traversal segments", path)
		}
	}
	return nil
}

func ensureParentDirectories(root, parent string) error {
	rel, err := filepath.Rel(root, parent)
	if err != nil {
		return err
	}
	if rel == "." {
		return nil
	}
	current := root
	for _, segment := range strings.Split(rel, string(filepath.Separator)) {
		current = filepath.Join(current, segment)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			if err := os.Mkdir(current, 0o755); err != nil && !os.IsExist(err) {
				return err
			}
			info, err = os.Lstat(current)
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing to create shared path through non-directory parent %s", current)
		}
	}
	return nil
}

func isExampleEnvFile(path string) bool {
	name := filepath.Base(path)
	if !strings.HasPrefix(name, ".env.") {
		return false
	}
	for _, suffix := range exampleEnvSuffixes {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

func isTracked(repoRoot, path string) bool {
	cmd := exec.Command("git", "-C", repoRoot, "ls-files", "--error-unmatch", "--", filepath.FromSlash(path))
	return cmd.Run() == nil
}

func matchesAny(path string, patterns []string) bool {
	for _, pattern := range patterns {
		if matched, _ := filepath.Match(filepath.FromSlash(pattern), filepath.FromSlash(path)); matched {
			return true
		}
		if path == pattern {
			return true
		}
	}
	return false
}
