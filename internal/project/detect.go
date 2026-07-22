package project

import (
	"os"
	"path/filepath"
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

// signature pairs a project type with a lockfile whose presence at a
// repo's root identifies it. Order matters: pnpm-lock.yaml is checked
// before package-lock.json so a pnpm project that also carries a stale
// npm lockfile still resolves to PNPM.
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
	{Python, "requirements.txt"},
	{Go, "go.mod"},
}

// Detect identifies repoRoot's project type by checking for known
// lockfiles, in signature order. Returns Unknown if none are found —
// callers should fall back to the built-in dependency-dir defaults
// rather than treating this as an error.
func Detect(repoRoot string) Type {
	for _, sig := range signatures {
		if fileExists(filepath.Join(repoRoot, sig.lockfile)) {
			return sig.projectType
		}
	}
	return Unknown
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
