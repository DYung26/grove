// Package pool manages the Btrfs loopback pool Grove uses to get
// copy-on-write clones on filesystems (like ext4) that don't support
// reflinks natively.
package pool

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/dyung/grove/internal/output"
	"golang.org/x/sys/unix"
)

const (
	DefaultSizeGB     = 12
	groveDir          = ".grove"
	imageFileName     = "pool.img"
	mountDirName      = "pool-mount"
	fstabPath         = "/etc/fstab"
	maxUsableFraction = 0.7
)

type Paths struct {
	Image      string
	MountPoint string
}

func DefaultPaths() (Paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, err
	}

	root := filepath.Join(home, groveDir)
	return Paths{
		Image:      filepath.Join(root, imageFileName),
		MountPoint: filepath.Join(root, mountDirName),
	}, nil
}

func IsMounted(mountPoint string) (bool, error) {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return false, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) > 1 && fields[1] == mountPoint {
			return true, nil
		}
	}
	return false, scanner.Err()
}

func Init(paths Paths, sizeGB uint64) error {
	mounted, err := IsMounted(paths.MountPoint)
	if err != nil {
		return err
	}
	if mounted {
		fmt.Printf("%s Pool already mounted at %s\n", output.Success("✓"), output.Path(paths.MountPoint))
		return nil
	}

	if err := reclaimOwnershipIfNeeded(filepath.Dir(paths.Image)); err != nil {
		return err
	}

	if err := ensureBtrfsProgs(); err != nil {
		return fmt.Errorf("install btrfs-progs: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(paths.Image), 0o755); err != nil {
		return err
	}

	requestedBytes := sizeGB << 30
	if err := checkAvailableSpace(filepath.Dir(paths.Image), requestedBytes, sizeGB); err != nil {
		return err
	}

	if !fileExists(paths.Image) {
		if err := createSparseFile(paths.Image, requestedBytes); err != nil {
			return err
		}
		if err := runSudo("mkfs.btrfs", "-f", paths.Image); err != nil {
			return err
		}
	}

	if err := os.MkdirAll(paths.MountPoint, 0o755); err != nil {
		return err
	}

	if err := runSudo("mount", "-o", "loop", paths.Image, paths.MountPoint); err != nil {
		return err
	}

	if err := chownToCurrentUser(paths.MountPoint); err != nil {
		return err
	}

	if err := ensureFstabEntry(paths); err != nil {
		return err
	}

	fmt.Printf("%s Pool mounted at %s (%dG)\n", output.Success("✓"), output.Path(paths.MountPoint), sizeGB)
	return nil
}

func ensureBtrfsProgs() error {
	return ensureCommand("mkfs.btrfs", "btrfs-progs", map[string]string{"zypper": "btrfsprogs"})
}

// reclaimOwnershipIfNeeded chowns root (recursively) back to the current
// user if it's owned by someone else. systemd creates missing fstab
// mountpoints (and their parent directories) as root while processing
// /etc/fstab at boot, before Grove itself ever touches them — the entry
// ensureFstabEntry writes for paths.MountPoint can trigger exactly that
// if ~/.grove didn't exist yet on an earlier boot. Left unfixed, that
// leaves plain unprivileged mkdir/file calls under ~/.grove failing with
// permission denied on every later `grove pool init`.
func reclaimOwnershipIfNeeded(root string) error {
	info, err := os.Stat(root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}

	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) == os.Getuid() {
		return nil
	}

	fmt.Printf("%s is owned by another user (likely created by systemd while processing the fstab entry); reclaiming ownership.\n", output.Path(root))
	owner := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	return runSudo("chown", "-R", owner, root)
}

func checkAvailableSpace(dir string, requestedBytes, sizeGB uint64) error {
	var stat unix.Statfs_t
	if err := unix.Statfs(dir, &stat); err != nil {
		return err
	}

	availableBytes := stat.Bavail * uint64(stat.Bsize)
	limit := float64(availableBytes) * maxUsableFraction
	if float64(requestedBytes) > limit {
		availableGB := float64(availableBytes) / (1 << 30)
		return fmt.Errorf("requested %dG exceeds %.0f%% of available space (%.1fG free); choose a smaller --size", sizeGB, maxUsableFraction*100, availableGB)
	}
	return nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func createSparseFile(path string, size uint64) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	return f.Truncate(int64(size))
}

func chownToCurrentUser(path string) error {
	owner := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	return runSudo("chown", owner, path)
}

func ensureFstabEntry(paths Paths) error {
	data, err := os.ReadFile(fstabPath)
	if err != nil {
		return err
	}
	if strings.Contains(string(data), paths.MountPoint) {
		return nil
	}

	entry := fmt.Sprintf("%s %s btrfs loop,defaults,nofail 0 0\n", paths.Image, paths.MountPoint)
	cmd := exec.Command("sudo", "tee", "-a", fstabPath)
	cmd.Stdin = strings.NewReader(entry)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func runSudo(args ...string) error {
	cmd := exec.Command("sudo", args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
