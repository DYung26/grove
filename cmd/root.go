package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/dyung/grove/internal/pool"
	"github.com/dyung/grove/internal/storage"

	"github.com/spf13/cobra"
)

var mutationLock *storage.Lock

var rootCmd = &cobra.Command{
	Use:                "grove",
	Short:              "Grove manages Git worktrees with fast, disk-efficient environments",
	SilenceErrors:      true,
	SilenceUsage:       true,
	PersistentPreRunE:  enforceStorageProtection,
	PersistentPostRunE: releaseStorageLock,
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		if storage.IsStorageError(err) {
			err = fmt.Errorf("%w: storage is exhausted or read-only; run `grove pool status`, then `grove clean` / `grove cleanup` or `grove pool repair` as appropriate", err)
		}
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

// exactArgs returns a cobra.PositionalArgs validator requiring exactly n
// arguments, like cobra.ExactArgs(n), but on a mismatch reports cmd.Use
// (e.g. "create <branch>", "rename <old-name> <new-name>") instead of
// Cobra's default "accepts N arg(s), received M" — that default never
// says what the missing or extra argument actually is, which matters
// most for commands whose Use string names more than one positional
// argument (rename's old-name/new-name are easy to conflate with each
// other or with the worktree's branch) but is worth the same clarity
// even for a single argument (create <branch> failing with just "accepts
// 1 arg(s), received 0" doesn't say the missing thing is a branch).
// Every command taking positional arguments uses this instead of
// cobra.ExactArgs directly, so the message is consistent across all of
// them rather than only the ones that happened to need it fixed first.
func exactArgs(n int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) != n {
			return fmt.Errorf("usage: %s", cmd.Use)
		}
		return nil
	}
}

func enforceStorageProtection(cmd *cobra.Command, args []string) error {
	mutating := isMutatingCommand(cmd)
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	paths, err := pool.DefaultPaths()
	if err != nil {
		return err
	}
	if mutating {
		lock, err := storage.Acquire(filepath.Join(home, ".grove", "storage-protection.lock"))
		if err != nil {
			return err
		}
		mutationLock = lock
	}
	mounted, err := pool.IsMounted(paths.MountPoint)
	if err != nil {
		if mutating {
			return err
		}
		fmt.Fprintln(os.Stderr, "Warning: storage mount state could not be checked:", err)
		return nil
	}
	report, err := storage.Inspect(filepath.Dir(paths.Image), paths.MountPoint, mounted)
	if err != nil {
		if mutating {
			return err
		}
		fmt.Fprintln(os.Stderr, "Warning: storage capacity could not be fully checked:", err)
		return nil
	}
	for _, warning := range report.Warnings() {
		fmt.Fprintln(os.Stderr, "Warning:", warning)
	}
	if !mutating {
		return nil
	}
	return report.ErrorForMutation(isStorageIntensiveCommand(cmd), isRecoveryCommand(cmd, args), allowsReadOnlyPool(cmd))
}

func releaseStorageLock(cmd *cobra.Command, args []string) error {
	if mutationLock == nil {
		return nil
	}
	err := mutationLock.Release()
	mutationLock = nil
	return err
}

func isMutatingCommand(cmd *cobra.Command) bool {
	switch cmd.CommandPath() {
	case "grove", "grove list", "grove status", "grove savings", "grove open", "grove pool status", "grove completion":
		return false
	default:
		return cmd.Parent() != nil && cmd.Name() != "help"
	}
}

func isStorageIntensiveCommand(cmd *cobra.Command) bool {
	switch cmd.CommandPath() {
	case "grove create", "grove repair", "grove pool init", "grove pool migrate", "grove pool resize":
		return true
	default:
		return false
	}
}

func allowsReadOnlyPool(cmd *cobra.Command) bool {
	switch cmd.CommandPath() {
	case "grove cleanup", "grove prune", "grove pool repair":
		return true
	default:
		return false
	}
}

func recheckBackingCapacity() error {
	paths, err := pool.DefaultPaths()
	if err != nil {
		return err
	}
	report, err := storage.Inspect(filepath.Dir(paths.Image), paths.MountPoint, false)
	if err != nil {
		return err
	}
	for _, warning := range report.Warnings() {
		fmt.Fprintln(os.Stderr, "Warning:", warning)
	}
	return report.ErrorForMutation(true, false, false)
}

func recheckStorageCapacity() error {
	paths, err := pool.DefaultPaths()
	if err != nil {
		return err
	}
	mounted, err := pool.IsMounted(paths.MountPoint)
	if err != nil {
		return err
	}
	report, err := storage.Inspect(filepath.Dir(paths.Image), paths.MountPoint, mounted)
	if err != nil {
		return err
	}
	for _, warning := range report.Warnings() {
		fmt.Fprintln(os.Stderr, "Warning:", warning)
	}
	return report.ErrorForMutation(true, false, false)
}

func isRecoveryCommand(cmd *cobra.Command, args []string) bool {
	if cmd.CommandPath() == "grove pool resize" && len(args) == 1 && strings.HasPrefix(args[0], "+") {
		return true
	}
	switch cmd.CommandPath() {
	case "grove clean", "grove cleanup", "grove remove", "grove prune", "grove pool repair":
		return true
	default:
		return false
	}
}
