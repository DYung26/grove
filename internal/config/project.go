package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// ProjectConfig is optional, per-repo, hand-authored settings meant to be
// committed to the repo (unlike Registry, which is local runtime state).
// It extends Grove's built-in dependency-dir detection rather than
// replacing it: DependencyDirs adds to the built-in set, and
// ExcludeDependencyDirs removes entries even if Grove would otherwise
// detect them.
type ProjectConfig struct {
	DependencyDirs        []string `json:"dependency_dirs,omitempty"`
	ExcludeDependencyDirs []string `json:"exclude_dependency_dirs,omitempty"`
}

func LoadProjectConfig(repoRoot string) (ProjectConfig, error) {
	data, err := os.ReadFile(projectConfigPath(repoRoot))
	if os.IsNotExist(err) {
		return ProjectConfig{}, nil
	}
	if err != nil {
		return ProjectConfig{}, err
	}

	var cfg ProjectConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return ProjectConfig{}, fmt.Errorf("parse %s: %w", projectConfigPath(repoRoot), err)
	}
	return cfg, nil
}

// ResolveDependencyDirs merges Grove's built-in defaults with a project's
// overrides: additions are unioned in, exclusions are removed even if
// they came from the built-in list. Result is sorted for stable output.
func ResolveDependencyDirs(builtin []string, cfg ProjectConfig) []string {
	included := make(map[string]bool)
	for _, dir := range builtin {
		included[dir] = true
	}
	for _, dir := range cfg.DependencyDirs {
		included[dir] = true
	}
	for _, dir := range cfg.ExcludeDependencyDirs {
		delete(included, dir)
	}

	dirs := make([]string, 0, len(included))
	for dir := range included {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	return dirs
}

func projectConfigPath(repoRoot string) string {
	return filepath.Join(repoRoot, ".grove.json")
}
