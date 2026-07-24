package fs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCheckHealthReturnsHealthyForAnOrdinaryReadableDirectory(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.txt"), "hello")

	if got := CheckHealth(dir); got != Healthy {
		t.Errorf("CheckHealth(%s) = %v, want Healthy", dir, got)
	}
}

func TestCheckHealthReturnsMissingForANonexistentPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist")

	if got := CheckHealth(path); got != Missing {
		t.Errorf("CheckHealth(%s) = %v, want Missing", path, got)
	}
}

func TestCheckHealthReturnsUnreadableWhenDirectoryPermissionsBlockListing(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission checks don't apply when running as root")
	}

	dir := t.TempDir()
	sub := filepath.Join(dir, "locked")
	mustMkdirAll(t, sub)
	if err := os.Chmod(sub, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	defer os.Chmod(sub, 0o755)

	if got := CheckHealth(sub); got != Unreadable {
		t.Errorf("CheckHealth(%s) = %v, want Unreadable", sub, got)
	}
}

func TestHealthStatusStringReturnsExpectedLabels(t *testing.T) {
	cases := map[HealthStatus]string{
		Healthy:    "healthy",
		Missing:    "missing",
		Unreadable: "unreadable",
		ReadOnly:   "read-only",
	}
	for status, want := range cases {
		if got := status.String(); got != want {
			t.Errorf("%v.String() = %q, want %q", int(status), got, want)
		}
	}
}

func TestIsReadOnlyOptionMatchesRoAmongCommaSeparatedOptions(t *testing.T) {
	cases := map[string]bool{
		"rw,relatime":         false,
		"ro,relatime":         true,
		"relatime,ro":         true,
		"rw":                  false,
		"noatime,ro,compress": true,
	}
	for options, want := range cases {
		if got := isReadOnlyOption(options); got != want {
			t.Errorf("isReadOnlyOption(%q) = %v, want %v", options, got, want)
		}
	}
}
