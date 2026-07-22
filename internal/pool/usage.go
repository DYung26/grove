package pool

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// DirUsage reports how much space a dependency dir is really costing,
// versus what it would cost if every byte were a fully independent copy.
type DirUsage struct {
	Path      string
	Apparent  uint64 // size if dir were not shared with anything else
	Exclusive uint64 // size actually attributable to this dir alone
}

// Saved reports how many bytes this dir avoided allocating, relative to
// a full independent copy.
func (u DirUsage) Saved() uint64 {
	if u.Apparent <= u.Exclusive {
		return 0
	}
	return u.Apparent - u.Exclusive
}

// MeasureDirUsage reports dir's apparent and exclusive usage. dir is
// resolved past any symlink first: Grove always turns a pool-resident
// dependency dir into a symlink pointing at its real backing directory
// (see pool.EnsureDependencyDirVolume), and `du`/`stat -f` on the
// symlink itself would just report the few-byte size of the stored
// link target string, not the real tree it points to. It only consults
// `btrfs filesystem du` when the resolved dir actually lives on a btrfs
// filesystem (a plain ext4 sibling .wt directory has no such notion,
// and running the command there would just fail); otherwise Exclusive
// falls back to hardlink-aware measurement.
func MeasureDirUsage(dir string) (DirUsage, error) {
	realDir, err := resolveSymlink(dir)
	if err != nil {
		return DirUsage{}, err
	}

	apparent, err := dirApparentSize(realDir)
	if err != nil {
		return DirUsage{}, err
	}
	usage := DirUsage{Path: dir, Apparent: apparent, Exclusive: apparent}

	onBtrfs, err := isBtrfs(realDir)
	if err != nil {
		return DirUsage{}, err
	}
	if onBtrfs {
		exclusive, err := btrfsExclusiveSize(realDir)
		if err != nil {
			return DirUsage{}, err
		}
		usage.Exclusive = exclusive
		return usage, nil
	}

	// Not on btrfs, so there's no reflink-based sharing to account for —
	// but the dir may still have been populated via Grove's hardlink
	// fallback (see fs.CloneTreeHardlinkOrCopy), which shares real disk
	// blocks with its clone source even without CoW. Without this, every
	// hardlinked dependency dir would report zero savings.
	exclusive, err := hardlinkExclusiveSize(realDir)
	if err != nil {
		return DirUsage{}, err
	}
	usage.Exclusive = exclusive
	return usage, nil
}

// resolveSymlink follows dir past a single (or chained) symlink to its
// real backing path, so callers measure the actual data instead of the
// symlink's own tiny size. A dir that isn't a symlink at all (e.g. a
// plain worktree dependency dir that was hardlinked or copied rather
// than pool-migrated) is returned unchanged.
func resolveSymlink(dir string) (string, error) {
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", dir, err)
	}
	return resolved, nil
}

// hardlinkExclusiveSize sums the size of dir's regular files that this
// dir owns exclusively (Nlink == 1), skipping any that are hardlinked
// elsewhere (Nlink > 1) — those share the same inode and blocks as their
// clone source, so they cost effectively nothing extra and shouldn't be
// counted toward this dir's exclusive usage. Directories and symlinks
// contribute nothing, matching how btrfsExclusiveSize only accounts for
// file data.
func hardlinkExclusiveSize(dir string) (uint64, error) {
	var total uint64
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil
		}

		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Nlink <= 1 {
			total += uint64(info.Size())
		}
		return nil
	})
	return total, err
}

func dirApparentSize(dir string) (uint64, error) {
	out, err := exec.Command("du", "-sb", dir).Output()
	if err != nil {
		return 0, fmt.Errorf("du %s: %w", dir, err)
	}

	fields := strings.Fields(string(out))
	if len(fields) < 1 {
		return 0, fmt.Errorf("unexpected du output for %s: %q", dir, out)
	}
	return strconv.ParseUint(fields[0], 10, 64)
}

func isBtrfs(dir string) (bool, error) {
	out, err := exec.Command("stat", "-f", "-c", "%T", dir).Output()
	if err != nil {
		return false, fmt.Errorf("stat -f %s: %w", dir, err)
	}
	return strings.TrimSpace(string(out)) == "btrfs", nil
}

// btrfsExclusiveSize sums the Exclusive column `btrfs filesystem du -s`
// reports for dir. Different btrfs-progs versions disagree on whether
// `du` even has a raw-bytes flag (some accept -b/--raw, some error with
// "invalid option"), so this deliberately doesn't pass one and instead
// parses the default human-readable column ("707.66MiB", "0.00B"),
// which every version produces. This is a read-only inspection of
// extent metadata and doesn't require root, unlike the mount/mkfs
// operations elsewhere in this package.
func btrfsExclusiveSize(dir string) (uint64, error) {
	cmd := exec.Command("btrfs", "filesystem", "du", "-s", dir)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	out, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("btrfs filesystem du %s: %w: %s", dir, err, strings.TrimSpace(stderr.String()))
	}

	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		exclusive, err := parseBtrfsSize(fields[1])
		if err != nil {
			continue // header row or anything else non-numeric
		}
		return exclusive, nil
	}
	return 0, fmt.Errorf("no usable output from btrfs filesystem du for %s", dir)
}

// btrfsSizeUnits maps the binary-unit suffixes `btrfs filesystem du`
// prints to their byte multiplier.
var btrfsSizeUnits = map[string]uint64{
	"B":   1,
	"KiB": 1 << 10,
	"MiB": 1 << 20,
	"GiB": 1 << 30,
	"TiB": 1 << 40,
	"PiB": 1 << 50,
}

// parseBtrfsSize parses a human-readable size string as printed by
// `btrfs filesystem du` (e.g. "707.66MiB", "0.00B") into raw bytes, by
// splitting off the trailing unit suffix and applying its multiplier.
func parseBtrfsSize(s string) (uint64, error) {
	i := len(s)
	for i > 0 && !(s[i-1] >= '0' && s[i-1] <= '9') && s[i-1] != '.' {
		i--
	}
	numPart, unitPart := s[:i], s[i:]

	mult, ok := btrfsSizeUnits[unitPart]
	if !ok {
		return 0, fmt.Errorf("unrecognized unit %q in size %q", unitPart, s)
	}
	value, err := strconv.ParseFloat(numPart, 64)
	if err != nil {
		return 0, fmt.Errorf("parse numeric part of %q: %w", s, err)
	}
	return uint64(value * float64(mult)), nil
}
