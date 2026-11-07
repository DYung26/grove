package cmd

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dyung/grove/internal/config"
	"github.com/dyung/grove/internal/git"
	"github.com/dyung/grove/internal/output"
	"github.com/spf13/cobra"
)

var cleanupYes bool

var cleanupCmd = &cobra.Command{
	Use:   "cleanup",
	Short: "Review and remove .grove-bak backups left by repair and pool migrations",
	RunE:  runCleanup,
}

func init() {
	cleanupCmd.Flags().BoolVar(&cleanupYes, "yes", false, "delete every backup without prompting")
	rootCmd.AddCommand(cleanupCmd)
}

// runCleanup reviews the registry's recorded backups oldest-first and
// prompts once per backup rather than once for the whole batch, since
// backups from very different ages carry very different risk. --yes
// skips every prompt.
func runCleanup(cmd *cobra.Command, args []string) error {
	repoRoot, err := git.MainRepoRoot()
	if err != nil {
		return err
	}

	reg, err := config.Load(repoRoot)
	if err != nil {
		return err
	}

	if len(reg.Backups) == 0 {
		fmt.Println(output.Success("✓") + " No backups to clean up.")
		return nil
	}

	backups := make([]config.BackupEntry, len(reg.Backups))
	copy(backups, reg.Backups)
	sort.Slice(backups, func(i, j int) bool {
		return backups[i].CreatedAt.Before(backups[j].CreatedAt)
	})

	reader := bufio.NewReader(os.Stdin)
	deleted, kept, quit := 0, 0, false

	for i, b := range backups {
		if quit {
			kept++
			continue
		}

		path, err := resolveBackupPath(repoRoot, reg, b)
		if err != nil {
			fmt.Println(output.Warn(fmt.Sprintf("  cannot resolve %s: %s (retained in registry)", output.Path(b.Path), err)))
			kept++
			continue
		}
		b.Path = path
		fmt.Println(describeBackup(b))

		remove := cleanupYes
		if !cleanupYes {
			remove, quit, err = promptCleanupChoice(reader, i, len(backups))
			if err != nil {
				return err
			}
		}

		if !remove {
			kept++
			continue
		}

		if err := removeBackup(b.Path); err != nil {
			fmt.Println(output.Warn(fmt.Sprintf("  failed to delete %s: %s (retained in registry)", output.Path(b.Path), err)))
			kept++
			continue
		}

		reg.RemoveBackup(backups[i].Path)
		deleted++
		fmt.Println(output.Success("  ✓ deleted"))
	}

	if err := reg.Save(repoRoot); err != nil {
		return err
	}

	fmt.Printf("\n%s Deleted %d, kept %d.\n", output.Success("✓"), deleted, kept)
	return nil
}

func resolveBackupPath(repoRoot string, reg *config.Registry, backup config.BackupEntry) (string, error) {
	if backup.Path == "" || !strings.HasSuffix(filepath.Base(backup.Path), ".grove-bak") {
		return "", fmt.Errorf("invalid backup path")
	}

	if filepath.IsAbs(backup.Path) {
		path := filepath.Clean(backup.Path)
		if _, err := os.Lstat(path); err != nil {
			return "", err
		}
		return path, nil
	}

	if !filepath.IsLocal(backup.Path) {
		return "", fmt.Errorf("relative path escapes its owning directory")
	}

	roots := []string{repoRoot}
	for _, worktree := range reg.Worktrees {
		if worktree.Path != "" {
			roots = append(roots, worktree.Path)
		}
	}

	matches := make(map[string]struct{})
	for _, root := range roots {
		path := filepath.Clean(filepath.Join(root, backup.Path))
		if _, err := os.Lstat(path); err == nil {
			matches[path] = struct{}{}
		} else if !os.IsNotExist(err) {
			return "", err
		}
	}

	if len(matches) == 0 {
		return "", fmt.Errorf("no matching backup found under the repository or its tracked worktrees")
	}
	if len(matches) > 1 {
		return "", fmt.Errorf("matches multiple tracked locations")
	}
	for path := range matches {
		return path, nil
	}
	return "", fmt.Errorf("backup path could not be resolved")
}

func removeBackup(path string) error {
	if _, err := os.Lstat(path); err != nil {
		return fmt.Errorf("backup does not exist: %w", err)
	}
	if err := os.RemoveAll(path); err != nil {
		return err
	}
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("path still exists after removal")
	} else if !os.IsNotExist(err) {
		return err
	}
	return nil
}

// describeBackup measures size live rather than caching it at creation,
// since a stale cached number is one more thing that could drift from
// reality.
func describeBackup(b config.BackupEntry) string {
	age := formatAge(time.Since(b.CreatedAt))
	size := "size unknown"
	if bytes, err := dirSize(b.Path); err == nil {
		size = formatBytes(bytes)
	}
	return fmt.Sprintf("%s\n  from %s, %s old, %s", output.Path(b.Path), b.Source, age, size)
}

// promptCleanupChoice asks y/N/a(ll)/q(uit) about a single backup: a
// switches every remaining backup this run to delete-without-asking, q
// stops asking and leaves the rest untouched. Bare enter defaults to no.
func promptCleanupChoice(reader *bufio.Reader, index, total int) (remove, quit bool, err error) {
	fmt.Printf("Delete? [%d/%d] [y/N/a/q] ", index+1, total)

	line, err := reader.ReadString('\n')
	if err != nil {
		return false, false, err
	}

	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, false, nil
	case "a", "all":
		cleanupYes = true
		return true, false, nil
	case "q", "quit":
		return false, true, nil
	default:
		return false, false, nil
	}
}

// formatAge renders a duration as the coarsest useful unit: minutes,
// hours, then days once a backup is at least a day old.
func formatAge(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// dirSize reports path's apparent size in bytes via `du -sb`. Unlike
// internal/pool/usage.go's MeasureDirUsage, this works against a plain
// directory rather than requiring a Btrfs subvolume, since every backup
// path here is a plain renamed-away directory, not a subvolume.
func dirSize(path string) (uint64, error) {
	out, err := exec.Command("du", "-sb", path).Output()
	if err != nil {
		return 0, fmt.Errorf("du %s: %w", path, err)
	}

	fields := strings.Fields(string(out))
	if len(fields) < 1 {
		return 0, fmt.Errorf("unexpected du output for %s: %q", path, out)
	}
	return strconv.ParseUint(fields[0], 10, 64)
}
