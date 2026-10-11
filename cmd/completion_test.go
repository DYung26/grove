package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestPrefixMatchesDeduplicatesAndFilters(t *testing.T) {
	got := prefixMatches([]string{"feature-a", "feature-b", "feature-a", "main"}, "feature-")
	if strings.Join(got, ",") != "feature-a,feature-b" {
		t.Fatalf("prefixMatches() = %v, want [feature-a feature-b]", got)
	}
}

func TestCompletionInstallTarget(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// os.UserHomeDir uses HOME on Unix and USERPROFILE on Windows.
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", home)
	}

	cases := []struct {
		shell string
		name  string
	}{
		{"bash", "grove"},
		{"zsh", "_grove"},
		{"fish", "grove.fish"},
		{"powershell", "grove.ps1"},
	}
	for _, tc := range cases {
		t.Run(tc.shell, func(t *testing.T) {
			dir, file, err := completionInstallTarget(tc.shell)
			if err != nil {
				t.Fatal(err)
			}
			if filepath.Base(file) != tc.name || filepath.Dir(file) != dir {
				t.Fatalf("target = (%q, %q), unexpected", dir, file)
			}
		})
	}
	if _, _, err := completionInstallTarget("tcsh"); err == nil {
		t.Fatal("expected unsupported shell error")
	}
}

func TestGenerateCompletionScriptsForAllShells(t *testing.T) {
	for _, shell := range []string{"bash", "zsh", "fish", "powershell"} {
		t.Run(shell, func(t *testing.T) {
			content, err := generateCompletionScript(shell)
			if err != nil {
				t.Fatal(err)
			}
			if len(content) == 0 {
				t.Fatal("generated empty completion script")
			}
		})
	}
}

func TestInstallRefusesOverwriteWithoutForce(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", home)
	}
	dir, filename, err := completionInstallTarget("fish")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte("user content\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	oldShell, oldForce := installCompletionsShell, installCompletionsForce
	t.Cleanup(func() { installCompletionsShell, installCompletionsForce = oldShell, oldForce })
	installCompletionsShell, installCompletionsForce = "fish", false
	cmd := &cobra.Command{}
	cmd.Flags().String("shell", "", "")
	if err := runInstallCompletions(cmd, nil); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("runInstallCompletions error = %v, want overwrite refusal", err)
	}
	got, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "user content\n" {
		t.Fatalf("existing file changed without --force: %q", got)
	}
}

func TestRegisteredWorktreeCompletionUsesCurrentRepo(t *testing.T) {
	// The root command's completion callbacks read the repository from the
	// current working directory, so run this test in a temporary Git repo.
	originalDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	tempRepo := t.TempDir()
	for _, args := range [][]string{{"init", "-b", "main", tempRepo}, {"-C", tempRepo, "config", "user.name", "Completion Test"}, {"-C", tempRepo, "config", "user.email", "completion@example.invalid"}, {"-C", tempRepo, "config", "commit.gpgsign", "false"}} {
		cmd := exec.Command("git", args...)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	if err := os.WriteFile(filepath.Join(tempRepo, "README.md"), []byte("completion test repo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("git", "-C", tempRepo, "add", "README.md").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v: %s", err, output)
	}
	if output, err := exec.Command("git", "-C", tempRepo, "commit", "-m", "test: initialize repo").CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v: %s", err, output)
	}
	t.Cleanup(func() { _ = os.Chdir(originalDir) })
	if err := os.Chdir(tempRepo); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Join(tempRepo, ".grove"), 0o755); err != nil {
		t.Fatal(err)
	}
	registry := `{"worktrees":[{"name":"api-feature","path":"/tmp/api-feature","branch":"feature/api"},{"name":"ui-feature","path":"/tmp/ui-feature","branch":"feature/ui"}]}`
	if err := os.WriteFile(filepath.Join(tempRepo, ".grove", "registry.json"), []byte(registry), 0o644); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("git", "-C", tempRepo, "branch", "feature/api").CombinedOutput(); err != nil {
		t.Fatalf("create local branch: %v: %s", err, output)
	}

	worktreeNames, directive := completeWorktreeNames(nil, nil, "api")
	if directive != cobra.ShellCompDirectiveNoFileComp || strings.Join(worktreeNames, ",") != "api-feature" {
		t.Fatalf("completeWorktreeNames() = %v, directive %v", worktreeNames, directive)
	}
	openNames, _ := completeWorktreeNamesAndBranches(nil, nil, "feature/")
	if strings.Join(openNames, ",") != "feature/api" {
		t.Fatalf("completeWorktreeNamesAndBranches() = %v, want [feature/api]", openNames)
	}
}
