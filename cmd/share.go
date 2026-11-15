package cmd

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/dyung/grove/internal/config"
	"github.com/dyung/grove/internal/git"
	"github.com/dyung/grove/internal/output"
	"github.com/dyung/grove/internal/shared"
	"github.com/spf13/cobra"
)

var shareCmd = &cobra.Command{
	Use:               "share [name]",
	ValidArgsFunction: completeWorktreeNames,
	Short:             "Link local-only project files into existing worktrees",
	Args:              cobra.MaximumNArgs(1),
	RunE:              runShare,
}

func init() { rootCmd.AddCommand(shareCmd) }

func runShare(cmd *cobra.Command, args []string) error {
	repoRoot, err := git.MainRepoRoot()
	if err != nil {
		return err
	}
	cfg, err := config.LoadProjectConfig(repoRoot)
	if err != nil {
		return err
	}
	paths, err := shared.Resolve(repoRoot, cfg.SharedPaths, cfg.ExcludeSharedPaths)
	if err != nil {
		return err
	}
	if len(paths) == 0 {
		fmt.Println("No shared files found. Grove checks .env files and docs/_local by default; add shared_paths in .grove.json for other paths.")
		return nil
	}
	reg, err := config.Load(repoRoot)
	if err != nil {
		return err
	}
	worktrees := reg.Worktrees
	if len(args) == 1 {
		wt, found := reg.Find(args[0])
		if !found {
			return fmt.Errorf("no tracked worktree named %q (see grove list)", args[0])
		}
		worktrees = []config.Worktree{wt}
	}
	if len(worktrees) == 0 {
		fmt.Println("No tracked worktrees to reconcile.")
		return nil
	}
	reader := bufio.NewReader(os.Stdin)
	for _, wt := range worktrees {
		if _, err := os.Stat(wt.Path); os.IsNotExist(err) {
			fmt.Println(output.Warn(fmt.Sprintf("Skipping %s: worktree path is missing (%s).", wt.Name, wt.Path)))
			continue
		} else if err != nil {
			return fmt.Errorf("inspect worktree %s: %w", wt.Name, err)
		}
		fmt.Println(output.Bold(wt.Name))
		for _, rel := range paths {
			backup, linked, err := reconcileSharedPath(reader, repoRoot, wt.Path, rel)
			if err != nil {
				if backup != "" {
					recordErr := reg.AddBackup(backup, "share")
					if recordErr == nil {
						recordErr = reg.Save(repoRoot)
					}
					if recordErr != nil {
						return fmt.Errorf("share %s in %s: %w (also failed to record backup %s: %v)", rel, wt.Name, err, backup, recordErr)
					}
				}
				return fmt.Errorf("share %s in %s: %w", rel, wt.Name, err)
			}
			if !linked {
				fmt.Printf("  Kept or skipped %s; no changes made.\n", rel)
				continue
			}
			if backup != "" {
				if err := reg.AddBackup(backup, "share"); err != nil {
					return err
				}
				if err := reg.Save(repoRoot); err != nil {
					return err
				}
				fmt.Printf("  Linked %s; previous copy preserved at %s\n", rel, backup)
			} else {
				fmt.Printf("  Linked %s\n", rel)
			}
		}
	}
	return nil
}

func reconcileSharedPath(reader *bufio.Reader, repoRoot, worktreeRoot, rel string) (string, bool, error) {
	if _, err := shared.Link(repoRoot, worktreeRoot, rel, false); err == nil {
		return "", true, nil
	} else if !errors.Is(err, shared.ErrDestinationExists) {
		return "", false, err
	}
	fmt.Printf("  %s already exists in this worktree.\n", rel)
	fmt.Println("    [l] Link the main-repo copy and preserve this copy as .grove-bak")
	fmt.Println("    [k] Keep this worktree copy")
	fmt.Println("    [s] Skip for now")
	fmt.Print("  Choose [k]: ")
	answer, err := reader.ReadString(10)
	if err != nil {
		return "", false, err
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "l", "link":
		fmt.Printf("  Replace %s with a symlink? The existing copy will be preserved. [y/N]: ", rel)
		confirm, err := reader.ReadString(10)
		if err != nil {
			return "", false, err
		}
		if strings.ToLower(strings.TrimSpace(confirm)) != "y" && strings.ToLower(strings.TrimSpace(confirm)) != "yes" {
			return "", false, nil
		}
		backup, err := shared.Link(repoRoot, worktreeRoot, rel, true)
		return backup, err == nil, err
	case "s", "skip", "k", "keep", "":
		return "", false, nil
	default:
		return "", false, fmt.Errorf("unknown choice %q; run grove share again", strings.TrimSpace(answer))
	}
}

func shareNewWorktree(repoRoot, worktreeRoot string) error {
	cfg, err := config.LoadProjectConfig(repoRoot)
	if err != nil {
		return err
	}
	paths, err := shared.Resolve(repoRoot, cfg.SharedPaths, cfg.ExcludeSharedPaths)
	if err != nil {
		return err
	}
	for _, rel := range paths {
		if _, err := shared.Link(repoRoot, worktreeRoot, rel, false); err != nil {
			if errors.Is(err, shared.ErrDestinationExists) {
				fmt.Println(output.Warn(fmt.Sprintf("Kept existing %s in the new worktree; it was not overwritten.", filepath.ToSlash(rel))))
				continue
			}
			return fmt.Errorf("link shared path %s: %w", rel, err)
		}
		fmt.Println(output.Dim(fmt.Sprintf("Shared %s", filepath.ToSlash(rel))))
	}
	return nil
}
