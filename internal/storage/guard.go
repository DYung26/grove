package storage

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

const (
	defaultWarnPercent     = 85
	defaultBlockPercent    = 90
	defaultCriticalPercent = 95
)

type Capacity struct {
	Path             string
	UsedPercent      float64
	FreeBytes        uint64
	TotalBytes       uint64
	FreeInodes       uint64
	TotalInodes      uint64
	InodeUsedPercent float64
	ReadOnly         bool
}

type Report struct {
	Backing         Capacity
	Pool            *Capacity
	WarnPercent     int
	BlockPercent    int
	CriticalPercent int
}

type Lock struct{ file *os.File }

func Acquire(lockPath string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		file.Close()
		return nil, fmt.Errorf("another Grove mutation is active; retry after it finishes: %w", err)
	}
	return &Lock{file: file}, nil
}

func (l *Lock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	err := unix.Flock(int(l.file.Fd()), unix.LOCK_UN)
	closeErr := l.file.Close()
	l.file = nil
	if err != nil {
		return err
	}
	return closeErr
}

func Inspect(backingPath, poolPath string, mounted bool) (Report, error) {
	warn, err := threshold("GROVE_STORAGE_WARN_PERCENT", defaultWarnPercent)
	if err != nil {
		return Report{}, err
	}
	block, err := threshold("GROVE_STORAGE_BLOCK_PERCENT", defaultBlockPercent)
	if err != nil {
		return Report{}, err
	}
	critical, err := threshold("GROVE_STORAGE_CRITICAL_PERCENT", defaultCriticalPercent)
	if err != nil {
		return Report{}, err
	}
	if warn >= block || block >= critical {
		return Report{}, fmt.Errorf("storage thresholds must satisfy WARN < BLOCK < CRITICAL")
	}
	backing, err := inspectPath(backingPath)
	if err != nil {
		return Report{}, fmt.Errorf("inspect backing filesystem %s: %w", backingPath, err)
	}
	report := Report{Backing: backing, WarnPercent: warn, BlockPercent: block, CriticalPercent: critical}
	if mounted {
		pool, err := inspectPath(poolPath)
		if err != nil {
			return Report{}, fmt.Errorf("inspect pool filesystem %s: %w", poolPath, err)
		}
		report.Pool = &pool
	}
	return report, nil
}

func inspectPath(path string) (Capacity, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return Capacity{}, err
	}
	totalBytes := stat.Blocks * uint64(stat.Bsize)
	freeBytes := stat.Bavail * uint64(stat.Bsize)
	usedPercent := percentage(stat.Blocks-stat.Bavail, stat.Blocks)
	freeInodes := stat.Ffree
	readOnly, err := mountReadOnly(path)
	if err != nil {
		return Capacity{}, err
	}
	return Capacity{Path: path, UsedPercent: usedPercent, FreeBytes: freeBytes, TotalBytes: totalBytes, FreeInodes: freeInodes, TotalInodes: stat.Files, InodeUsedPercent: percentage(stat.Files-stat.Ffree, stat.Files), ReadOnly: readOnly}, nil
}

func threshold(name string, fallback int) (int, error) {
	value := os.Getenv(name)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 1 || parsed > 100 {
		return 0, fmt.Errorf("%s must be an integer from 1 to 100", name)
	}
	return parsed, nil
}

func percentage(used, total uint64) float64 {
	if total == 0 {
		return 0
	}
	return float64(used) * 100 / float64(total)
}

func mountReadOnly(path string) (bool, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false, fmt.Errorf("resolve filesystem path %s: %w", path, err)
	}
	mounts, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return false, fmt.Errorf("read mount table: %w", err)
	}
	return mountReadOnlyFrom(resolved, mounts)
}

func mountReadOnlyFrom(path string, mounts []byte) (bool, error) {
	best, readOnly := "", false
	for _, line := range strings.Split(string(mounts), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		mountPoint := unescapeMount(fields[1])
		if (path == mountPoint || strings.HasPrefix(path, strings.TrimRight(mountPoint, "/")+"/")) && len(mountPoint) > len(best) {
			best, readOnly = mountPoint, false
			for _, option := range strings.Split(fields[3], ",") {
				if option == "ro" {
					readOnly = true
					break
				}
			}
		}
	}
	if best == "" {
		return false, fmt.Errorf("no mount entry found for filesystem path %s", path)
	}
	return readOnly, nil
}

func unescapeMount(value string) string {
	replacer := strings.NewReplacer("\\040", " ", "\\011", "\t", "\\134", "\\", "\\012", "\n")
	return replacer.Replace(value)
}

func (c Capacity) FullPercent() float64 {
	if c.InodeUsedPercent > c.UsedPercent {
		return c.InodeUsedPercent
	}
	return c.UsedPercent
}

func (c Capacity) String() string {
	if c.TotalInodes == 0 {
		return fmt.Sprintf("%s: %.1f%% blocks used, %s bytes free; inode capacity not reported", c.Path, c.UsedPercent, formatBytes(c.FreeBytes))
	}
	return fmt.Sprintf("%s: %.1f%% blocks used, %s bytes free; %.1f%% inodes used, %d inodes free", c.Path, c.UsedPercent, formatBytes(c.FreeBytes), c.InodeUsedPercent, c.FreeInodes)
}

func (r Report) ErrorForMutation(storageIntensive, recovery, allowReadOnlyPool bool) error {
	capacities := []Capacity{r.Backing}
	if r.Pool != nil {
		capacities = append(capacities, *r.Pool)
	}
	for _, capacity := range capacities {
		if capacity.ReadOnly && !(allowReadOnlyPool && r.Pool != nil && capacity.Path == r.Pool.Path) {
			return fmt.Errorf("storage filesystem is read-only: %s. Stop writes and recover the filesystem before retrying", capacity.String())
		}
	}
	if recovery {
		return nil
	}
	for _, capacity := range capacities {
		usage := capacity.FullPercent()
		if capacity.FreeBytes == 0 || (capacity.TotalInodes > 0 && capacity.FreeInodes == 0) || usage >= float64(r.CriticalPercent) {
			return fmt.Errorf("critical storage threshold reached (%.1f%%; limit %d%%): %s. Mutation blocked to prevent ENOSPC. Recover with `grove clean`, `grove cleanup`, or grow the pool with `grove pool resize +N`", usage, r.CriticalPercent, capacity.String())
		}
		if storageIntensive && usage >= float64(r.BlockPercent) {
			return fmt.Errorf("storage-intensive operation blocked at %.1f%% usage (limit %d%%): %s. Free space with `grove clean` / `grove cleanup`, or grow the pool with `grove pool resize +N`", usage, r.BlockPercent, capacity.String())
		}
	}
	return nil
}

func (r Report) Warnings() []string {
	var warnings []string
	capacities := []Capacity{r.Backing}
	if r.Pool != nil {
		capacities = append(capacities, *r.Pool)
	}
	for _, capacity := range capacities {
		usage := capacity.FullPercent()
		if usage >= float64(r.WarnPercent) {
			warnings = append(warnings, fmt.Sprintf("storage warning at %.1f%% (warning threshold %d%%): %s. Free space with `grove clean` / `grove cleanup`, or grow the pool with `grove pool resize +N`", usage, r.WarnPercent, capacity.String()))
		}
	}
	return warnings
}

func IsStorageError(err error) bool {
	return errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EROFS)
}

func formatBytes(value uint64) string {
	const unit = 1024
	if value < unit {
		return fmt.Sprintf("%d B", value)
	}
	div, exp := uint64(unit), 0
	for n := value / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(value)/float64(div), "KMGTPE"[exp])
}
