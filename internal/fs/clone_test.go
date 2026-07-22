package fs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCloneTreeHardlinkOrCopyHardlinksRegularFilesAndPreservesStructure(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "dst")

	writeFile(t, filepath.Join(src, "a.txt"), "hello")
	mustMkdirAll(t, filepath.Join(src, "nested"))
	writeFile(t, filepath.Join(src, "nested", "b.txt"), "world")
	mustSymlink(t, "a.txt", filepath.Join(src, "link.txt"))

	mode, err := CloneTreeHardlinkOrCopy(src, dst, true)
	if err != nil {
		t.Fatalf("CloneTreeHardlinkOrCopy: %v", err)
	}
	if mode != CloneModeHardlink {
		t.Errorf("mode = %v, want CloneModeHardlink", mode)
	}

	assertFileContent(t, filepath.Join(dst, "a.txt"), "hello")
	assertFileContent(t, filepath.Join(dst, "nested", "b.txt"), "world")
	assertSymlinkTarget(t, filepath.Join(dst, "link.txt"), "a.txt")
	assertSameFile(t, filepath.Join(src, "a.txt"), filepath.Join(dst, "a.txt"))
}

func TestCloneTreeHardlinkOrCopyWithHardlinkOnlyFalseAlwaysCopies(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "dst")
	writeFile(t, filepath.Join(src, "a.txt"), "hello")

	mode, err := CloneTreeHardlinkOrCopy(src, dst, false)
	if err != nil {
		t.Fatalf("CloneTreeHardlinkOrCopy: %v", err)
	}
	if mode != CloneModeCopy {
		t.Errorf("mode = %v, want CloneModeCopy", mode)
	}

	assertFileContent(t, filepath.Join(dst, "a.txt"), "hello")
	assertNotSameFile(t, filepath.Join(src, "a.txt"), filepath.Join(dst, "a.txt"))
}

func TestCloneTreeReflinkOnlyRespectsFilesystemSupport(t *testing.T) {
	src := t.TempDir()
	dstParent := t.TempDir()
	dst := filepath.Join(dstParent, "dst")
	writeFile(t, filepath.Join(src, "a.txt"), "hello")

	reflinked, err := CloneTreeReflinkOnly(src, dst)

	if DetectCloneSupport(dstParent) != CloneSupported {
		if err == nil {
			t.Fatalf("expected an error on a filesystem without reflink support")
		}
		if !errors.Is(err, ErrCloneUnsupported) {
			t.Errorf("error = %v, want it to wrap ErrCloneUnsupported", err)
		}
		return
	}

	if err != nil {
		t.Fatalf("CloneTreeReflinkOnly: %v", err)
	}
	if !reflinked {
		t.Errorf("reflinked = false, want true")
	}
	assertFileContent(t, filepath.Join(dst, "a.txt"), "hello")
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mustMkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

func mustSymlink(t *testing.T, target, linkPath string) {
	t.Helper()
	if err := os.Symlink(target, linkPath); err != nil {
		t.Fatalf("symlink %s -> %s: %v", linkPath, target, err)
	}
}

func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(got) != want {
		t.Errorf("content of %s = %q, want %q", path, got, want)
	}
}

func assertSymlinkTarget(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.Readlink(path)
	if err != nil {
		t.Fatalf("readlink %s: %v", path, err)
	}
	if got != want {
		t.Errorf("symlink target of %s = %q, want %q", path, got, want)
	}
}

func assertSameFile(t *testing.T, a, b string) {
	t.Helper()
	infoA, err := os.Stat(a)
	if err != nil {
		t.Fatalf("stat %s: %v", a, err)
	}
	infoB, err := os.Stat(b)
	if err != nil {
		t.Fatalf("stat %s: %v", b, err)
	}
	if !os.SameFile(infoA, infoB) {
		t.Errorf("expected %s and %s to be hardlinked (same inode)", a, b)
	}
}

func assertNotSameFile(t *testing.T, a, b string) {
	t.Helper()
	infoA, err := os.Stat(a)
	if err != nil {
		t.Fatalf("stat %s: %v", a, err)
	}
	infoB, err := os.Stat(b)
	if err != nil {
		t.Fatalf("stat %s: %v", b, err)
	}
	if os.SameFile(infoA, infoB) {
		t.Errorf("expected %s and %s to be independent copies, not sharing an inode", a, b)
	}
}
