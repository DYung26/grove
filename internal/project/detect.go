package project

import (
	"os"
	"path/filepath"
	"sort"
)

// Type identifies the ecosystem a repo belongs to, so Grove can choose an
// appropriate dependency-cloning strategy per project instead of always
// assuming node_modules.
type Type int

const (
	Unknown Type = iota
	NPM
	PNPM
	Cargo
	Python
	Go
)

func (t Type) String() string {
	switch t {
	case NPM:
		return "npm"
	case PNPM:
		return "pnpm"
	case Cargo:
		return "cargo"
	case Python:
		return "python"
	case Go:
		return "go"
	default:
		return "unknown"
	}
}

// signature pairs a project type with a lockfile whose presence in a
// directory identifies it there. Order matters: pnpm-lock.yaml is
// checked before package-lock.json so a pnpm project that also carries a
// stale npm lockfile still resolves to PNPM.
type signature struct {
	projectType Type
	lockfile    string
}

var signatures = []signature{
	{PNPM, "pnpm-lock.yaml"},
	{NPM, "package-lock.json"},
	{Cargo, "Cargo.lock"},
	{Python, "poetry.lock"},
	{Python, "Pipfile.lock"},
	{Python, "uv.lock"},
	{Python, "requirements.txt"},
	{Go, "go.mod"},
}

// maxScanDepth bounds how far below repoRoot Scan descends looking for
// nested projects (repoRoot itself is depth 0). A monorepo's project
// subdirectories (e.g. core/, services/api/) are typically shallow;
// unbounded recursion risks wandering into a dependency dir some other
// subproject already produced (e.g. a nested node_modules containing
// its own vendored package-lock.json) and misreporting it as a sibling
// project. Skipped directories (see skipDirs) already prevent that
// specific case, but the depth bound is a second, independent guard
// against runaway repos in general.
const maxScanDepth = 3

// skipDirs names directories Scan never descends into: version control
// and Grove's own runtime state, plus the dependency dirs Grove itself
// manages, which can contain a vendored lockfile of their own (e.g. a
// stray package-lock.json inside node_modules) that would otherwise be
// misidentified as a separate project.
var skipDirs = map[string]bool{
	".git":         true,
	".grove":       true,
	"node_modules": true,
	"target":       true,
	".venv":        true,
	"venv":         true,
}

// Project is one lockfile-identified project found under a repo, at Dir
// relative to the repo root ("" for the repo root itself, "core" for a
// nested crate at <repoRoot>/core).
type Project struct {
	Dir  string
	Type Type
}

// Scan finds every project under repoRoot by walking down to maxScanDepth
// and checking each directory for a known lockfile, in signature order.
// A repo can contain more than one: a polyglot monorepo (e.g. a Rust
// crate under core/ alongside a plain-JS frontend with no lockfile at
// the repo root at all) has no single project type, so callers that
// need dependency dirs should resolve each returned Project's dirs
// relative to its own Dir rather than assuming everything hangs off
// repoRoot directly. Returns nil if no lockfile is found anywhere.
func Scan(repoRoot string) ([]Project, error) {
	var projects []Project
	err := scanDir(repoRoot, repoRoot, 0, &projects)
	if err != nil {
		return nil, err
	}

	sort.Slice(projects, func(i, j int) bool { return projects[i].Dir < projects[j].Dir })
	return projects, nil
}

func scanDir(repoRoot, dir string, depth int, projects *[]Project) error {
	if projectType, ok := detectAt(dir); ok {
		relDir, err := filepath.Rel(repoRoot, dir)
		if err != nil {
			return err
		}
		if relDir == "." {
			relDir = ""
		}
		*projects = append(*projects, Project{Dir: relDir, Type: projectType})
	}

	if depth >= maxScanDepth {
		return nil
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || skipDirs[entry.Name()] {
			continue
		}
		if err := scanDir(repoRoot, filepath.Join(dir, entry.Name()), depth+1, projects); err != nil {
			return err
		}
	}
	return nil
}

// detectAt identifies dir's project type by checking for known
// lockfiles directly inside it, in signature order. ok is false if none
// are found.
func detectAt(dir string) (Type, bool) {
	for _, sig := range signatures {
		if fileExists(filepath.Join(dir, sig.lockfile)) {
			return sig.projectType, true
		}
	}
	return Unknown, false
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

