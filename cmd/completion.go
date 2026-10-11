package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/dyung/grove/internal/config"
	"github.com/dyung/grove/internal/git"
	"github.com/spf13/cobra"
)

var installCompletionsShell string
var installCompletionsForce bool

var installCompletionsCmd = &cobra.Command{
	Use:   "install-completions [bash|zsh|fish|powershell]",
	Short: "Install Grove shell completions persistently",
	Long: `Install the generated shell-completion script into the current user's normal completion directory.

The existing "grove completion <shell>" command remains available for printing the script yourself.
When shell is omitted, Grove detects the current shell from SHELL (Unix) or defaults to PowerShell on Windows.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runInstallCompletions,
}

func init() {
	installCompletionsCmd.Flags().StringVar(&installCompletionsShell, "shell", "", "shell to install for (bash, zsh, fish, powershell)")
	installCompletionsCmd.Flags().BoolVar(&installCompletionsForce, "force", false, "replace an existing Grove completion file")
	rootCmd.AddCommand(installCompletionsCmd)
}

func runInstallCompletions(cmd *cobra.Command, args []string) error {
	shell := strings.ToLower(strings.TrimSpace(installCompletionsShell))
	if cmd.Flags().Changed("shell") && len(args) == 1 {
		return fmt.Errorf("specify shell either positionally or with --shell, not both")
	}
	if len(args) == 1 {
		shell = strings.ToLower(strings.TrimSpace(args[0]))
	}
	if shell == "" {
		shell = detectCompletionShell()
	}

	dir, filename, err := completionInstallTarget(shell)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filename); err == nil && !installCompletionsForce {
		return fmt.Errorf("completion file already exists at %s (use --force to replace it)", filename)
	} else if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("inspect completion file %s: %w", filename, err)
	}

	content, err := generateCompletionScript(shell)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create completion directory %s: %w", dir, err)
	}
	flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	if !installCompletionsForce {
		flags = os.O_WRONLY | os.O_CREATE | os.O_EXCL
	}
	file, err := os.OpenFile(filename, flags, 0o644)
	if err != nil {
		if os.IsExist(err) && !installCompletionsForce {
			return fmt.Errorf("completion file already exists at %s (use --force to replace it)", filename)
		}
		return fmt.Errorf("write completion file %s: %w", filename, err)
	}
	if _, err := file.Write(content); err != nil {
		file.Close()
		return fmt.Errorf("write completion file %s: %w", filename, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close completion file %s: %w", filename, err)
	}

	fmt.Printf("Installed Grove %s completions to %s\n", shell, filename)
	fmt.Println(completionActivationHint(shell))
	return nil
}

func detectCompletionShell() string {
	if runtime.GOOS == "windows" {
		return "powershell"
	}
	base := filepath.Base(os.Getenv("SHELL"))
	switch base {
	case "bash", "zsh", "fish":
		return base
	default:
		return "bash"
	}
}

func completionInstallTarget(shell string) (dir, filename string, err error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", fmt.Errorf("resolve home directory: %w", err)
	}
	switch shell {
	case "bash":
		if runtime.GOOS == "darwin" {
			if prefix := os.Getenv("HOMEBREW_PREFIX"); prefix != "" {
				dir = filepath.Join(prefix, "etc", "bash_completion.d")
			} else {
				dir = filepath.Join(home, ".local", "share", "bash-completion", "completions")
			}
		} else {
			dir = filepath.Join(home, ".local", "share", "bash-completion", "completions")
		}
		filename = filepath.Join(dir, "grove")
	case "zsh":
		dir = filepath.Join(home, ".zfunc")
		filename = filepath.Join(dir, "_grove")
	case "fish":
		dir = filepath.Join(home, ".config", "fish", "completions")
		filename = filepath.Join(dir, "grove.fish")
	case "powershell":
		if runtime.GOOS == "windows" {
			profileRoot := os.Getenv("USERPROFILE")
			if profileRoot == "" {
				profileRoot = home
			}
			dir = filepath.Join(profileRoot, "Documents", "PowerShell", "Completions")
		} else {
			dir = filepath.Join(home, ".config", "powershell", "completions")
		}
		filename = filepath.Join(dir, "grove.ps1")
	default:
		return "", "", fmt.Errorf("unsupported shell %q; choose bash, zsh, fish, or powershell", shell)
	}
	return dir, filename, nil
}

func generateCompletionScript(shell string) ([]byte, error) {
	var b strings.Builder
	var err error
	switch shell {
	case "bash":
		err = rootCmd.GenBashCompletionV2(&b, true)
	case "zsh":
		err = rootCmd.GenZshCompletion(&b)
	case "fish":
		err = rootCmd.GenFishCompletion(&b, true)
	case "powershell":
		err = rootCmd.GenPowerShellCompletionWithDesc(&b)
	default:
		return nil, fmt.Errorf("unsupported shell %q; choose bash, zsh, fish, or powershell", shell)
	}
	if err != nil {
		return nil, fmt.Errorf("generate %s completion: %w", shell, err)
	}
	return []byte(b.String()), nil
}

func completionActivationHint(shell string) string {
	switch shell {
	case "bash":
		if runtime.GOOS == "darwin" && os.Getenv("HOMEBREW_PREFIX") != "" {
			return "Open a new Bash session to load it. If it is not auto-loaded, add: source \"$HOMEBREW_PREFIX/etc/bash_completion.d/grove\" to ~/.bashrc."
		}
		return "Open a new Bash session to load it. On systems without bash-completion auto-loading, add: source \"$HOME/.local/share/bash-completion/completions/grove\" to ~/.bashrc."
	case "zsh":
		return "Add 'fpath=(\"$HOME/.zfunc\" $fpath)' before 'autoload -Uz compinit && compinit' in ~/.zshrc, then start a new Zsh session."
	case "fish":
		return "Fish loads files in its completions directory automatically; start a new Fish session."
	case "powershell":
		if runtime.GOOS == "windows" {
			return "Add '. $HOME/Documents/PowerShell/Completions/grove.ps1' to your PowerShell profile, then start a new session."
		}
		return "Add '. $HOME/.config/powershell/completions/grove.ps1' to your PowerShell profile, then start a new session."
	default:
		return "Start a new shell session to load completions."
	}
}

func completeLocalBranches(_ *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) != 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	branches, err := git.ListLocalBranches()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return prefixMatches(branches, toComplete), cobra.ShellCompDirectiveNoFileComp
}

func completeWorktreeNames(_ *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) != 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	worktrees, err := registeredWorktreeNames()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return prefixMatches(worktrees, toComplete), cobra.ShellCompDirectiveNoFileComp
}

func completeWorktreeNamesAndBranches(_ *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) != 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	worktrees, err := registeredWorktreeNames()
	if err != nil {
		worktrees = nil
	}
	branches, err := git.ListLocalBranches()
	if err == nil {
		worktrees = append(worktrees, branches...)
	}
	return prefixMatches(worktrees, toComplete), cobra.ShellCompDirectiveNoFileComp
}

func completeRenameArgs(_ *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	worktrees, err := registeredWorktreeNames()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return prefixMatches(worktrees, toComplete), cobra.ShellCompDirectiveNoFileComp
}

func registeredWorktreeNames() ([]string, error) {
	repoRoot, err := git.MainRepoRoot()
	if err != nil {
		return nil, err
	}
	reg, err := config.Load(repoRoot)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(reg.Worktrees))
	for _, wt := range reg.Worktrees {
		names = append(names, wt.Name)
	}
	return names, nil
}

func prefixMatches(values []string, prefix string) []string {
	seen := make(map[string]struct{}, len(values))
	matches := make([]string, 0, len(values))
	for _, value := range values {
		if !strings.HasPrefix(value, prefix) {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		matches = append(matches, value)
	}
	return matches
}
