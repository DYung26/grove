package cmd

import (
	"path/filepath"
	"testing"

	"github.com/dyung/grove/internal/fs"
)

func TestNativeReflinkProbeDirectory(t *testing.T) {
	repoRoot := filepath.Join(t.TempDir(), "project")
	workingDirectory := filepath.Join(t.TempDir(), "home")

	if got := nativeReflinkProbeDir(repoRoot, workingDirectory); got != filepath.Dir(repoRoot) {
		t.Fatalf("repo probe directory = %q, want %q", got, filepath.Dir(repoRoot))
	}
	if got := nativeReflinkProbeDir("", workingDirectory); got != workingDirectory {
		t.Fatalf("outside-repo probe directory = %q, want %q", got, workingDirectory)
	}
}

func TestDirectoryHasNativeReflink(t *testing.T) {
	for _, support := range []struct {
		name    string
		support fs.CloneSupport
		want    bool
	}{
		{name: "supported", support: fs.CloneSupported, want: true},
		{name: "unsupported", support: fs.CloneUnsupported, want: false},
		{name: "unknown", support: fs.CloneUnknown, want: false},
	} {
		t.Run(support.name, func(t *testing.T) {
			if got := directoryHasNativeReflink("/tmp", func(string) fs.CloneSupport {
				return support.support
			}); got != support.want {
				t.Fatalf("directoryHasNativeReflink() = %t, want %t", got, support.want)
			}
		})
	}
}
