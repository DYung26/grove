// Package pool manages the Btrfs loopback pool Grove uses to get
// copy-on-write clones on filesystems (like ext4) that don't support
// reflinks natively.
package pool

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

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

// IsReadOnly reports whether mountPoint is currently mounted read-only.
// Btrfs force-remounts a filesystem read-only, on its own, when it hits
// metadata corruption it can't safely continue writing past — this is
// what distinguishes "the pool itself has stopped accepting writes,
// nothing inside it can be created or deleted" from "one subvolume is
// unreadable but the pool otherwise still works fine", the two failure
// modes that need genuinely different fixes. Parses /proc/mounts
// directly, the same as IsMounted, rather than shelling out to `mount`.
func IsReadOnly(mountPoint string) (bool, error) {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return false, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 4 || fields[1] != mountPoint {
			continue
		}
		for _, opt := range strings.Split(fields[3], ",") {
			if opt == "ro" {
				return true, nil
			}
		}
		return false, nil
	}
	return false, fmt.Errorf("%s is not currently mounted", mountPoint)
}

// existingLoopDevices returns every /dev/loopN currently attached to image, via `losetup -j` (list, filtered to one backing file). This can legitimately return more than one entry: nothing in the kernel stops two independent loop devices from attaching to the same backing file, which normally only happens when a prior mount was torn down uncleanly (crash, forced unmount after an I/O error, a boot where the nofail fstab entry raced something) and left a loop device dangling without ever being detached. Init uses this to reuse a live attachment instead of blindly creating a new one, and to warn if it finds more than one already.
func existingLoopDevices(image string) ([]string, error) {
	out, err := exec.Command("losetup", "-j", image).Output()
	if err != nil {
		// losetup -j exits 0 with empty output when nothing matches, so a real error here is a genuine failure to ask the question, not "no attachments found".
		return nil, fmt.Errorf("losetup -j %s: %w", image, err)
	}

	var devices []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		// Each line looks like "/dev/loop1: []: (/home/user/.grove/pool.img)".
		device, _, found := strings.Cut(line, ":")
		if found {
			devices = append(devices, device)
		}
	}
	return devices, nil
}

func Init(paths Paths, sizeGB uint64) error {
	mounted, err := IsMounted(paths.MountPoint)
	if err != nil {
		return err
	}
	if mounted {
		if err := warnIfMultiplyMounted(paths.Image, paths.MountPoint); err != nil {
			return err
		}
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

	if err := mountImage(paths); err != nil {
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

// mountImage mounts paths.Image at paths.MountPoint, first checking whether the image file is already attached to a loop device rather than letting a bare `mount -o loop` decide that on its own. mount -o loop normally reuses an existing attachment for the same file when there's exactly one, but that's a courtesy the kernel provides, not a guarantee Init can rely on — if a prior mount was ever torn down uncleanly and left a loop device dangling, or if two attachments already exist from an earlier bug, proceeding blindly risks ending up with two independent loop devices mounted over the same backing file at once, which two Btrfs instances writing through independently can corrupt. Checking first turns that silent risk into a visible, actionable one.
func mountImage(paths Paths) error {
	devices, err := existingLoopDevices(paths.Image)
	if err != nil {
		return err
	}

	switch len(devices) {
	case 0:
		return runSudo("mount", "-o", "loop", paths.Image, paths.MountPoint)
	case 1:
		// Already attached (e.g. left over from an unclean shutdown) but not mounted anywhere, since Init already confirmed paths.MountPoint itself isn't mounted before calling this. Mount that same device directly instead of asking for a fresh attachment.
		fmt.Printf("%s %s is already attached to %s; mounting it directly instead of creating a new loop device.\n", output.Warn("Note:"), output.Path(paths.Image), devices[0])
		return runSudo("mount", devices[0], paths.MountPoint)
	default:
		return fmt.Errorf("%s is attached to %d loop devices at once (%s); this shouldn't happen and risks Btrfs corruption if more than one gets mounted — detach the extras with `sudo losetup -d <device>` after confirming with `lsof <device>`/`fuser <device>` that nothing is using them, then re-run `grove pool init`", paths.Image, len(devices), strings.Join(devices, ", "))
	}
}

// ListSubvolumes returns the name of every top-level subvolume
// currently in the pool at mountPoint (e.g. "assessly-backend-main",
// "assessly-backend-feat-x-node_modules"), via `btrfs subvolume list`.
// This is a read-only extent-metadata query, same class of operation as
// usage.go's btrfsExclusiveSize, and needs no root on most systems —
// but some kernel/mount-option combinations restrict subvolume listing
// to privileged users regardless, so a permission failure here is a
// real, recoverable case, not a can't-happen one. Runs unprivileged
// first and only escalates to sudo on a permission-shaped failure,
// rather than always prompting for a password on a read-only status
// check that usually doesn't need one.
func ListSubvolumes(mountPoint string) ([]string, error) {
	out, err := runBtrfsSubvolumeList(mountPoint, false)
	if err != nil && isPermissionDenied(err) {
		out, err = runBtrfsSubvolumeList(mountPoint, true)
	}
	if err != nil {
		return nil, err
	}

	var names []string
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		// Each line looks like: "ID 257 gen 12 top level 5 path assessly-backend-main".
		// The subvolume's path is everything after the last " path ", which
		// keeps this correct even if a subvolume name itself happened to
		// contain the substring "path ".
		line := scanner.Text()
		idx := strings.LastIndex(line, " path ")
		if idx == -1 {
			continue
		}
		names = append(names, line[idx+len(" path "):])
	}
	return names, scanner.Err()
}

// runBtrfsSubvolumeList runs `btrfs subvolume list mountPoint`, via sudo
// if asRoot is set, and returns stdout. On failure the error includes
// stderr — exec.Cmd.Output() alone discards it, which previously turned
// every real cause (most commonly "Permission denied", but also a
// mountPoint that isn't actually btrfs, or btrfs-progs missing
// mid-command) into an opaque "exit status 1" with no way to tell them
// apart.
func runBtrfsSubvolumeList(mountPoint string, asRoot bool) (string, error) {
	var cmd *exec.Cmd
	if asRoot {
		cmd = exec.Command("sudo", "btrfs", "subvolume", "list", mountPoint)
	} else {
		cmd = exec.Command("btrfs", "subvolume", "list", mountPoint)
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("btrfs subvolume list %s: %w: %s", mountPoint, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// isPermissionDenied reports whether err (as returned by
// runBtrfsSubvolumeList) looks like a permission failure rather than
// some other cause (bad path, btrfs-progs missing, etc.) that retrying
// with sudo wouldn't fix and would just prompt for a password
// needlessly. Matches on the wrapped stderr text rather than an exit
// code, since `btrfs subvolume list` doesn't document a distinct exit
// status for this case. Checks both phrasings btrfs-progs actually
// uses: a plain filesystem-permission failure surfaces as "Permission
// denied", but the far more common case for this specific command —
// confirmed live on this machine — is the tree-search ioctl itself
// requiring CAP_SYS_ADMIN regardless of file permissions, which btrfs
// reports as "ERROR: can't perform the search: Operation not
// permitted" instead. Both need the same fix (retry as root), so both
// are treated as the same case here.
func isPermissionDenied(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "permission denied") || strings.Contains(msg, "operation not permitted")
}

func ensureBtrfsProgs() error {
	return ensureCommand("mkfs.btrfs", "btrfs-progs", map[string]string{"zypper": "btrfsprogs"})
}

// mountsOf returns every mountpoint currently mounted from any of the given devices, by scanning /proc/mounts once and matching device against fields[0]. Used to see where a pool image's loop devices are actually mounted — IsMounted alone only answers "is Grove's own expected mountpoint in use", which says nothing about a completely separate mountpoint (e.g. one set up by hand while testing, outside ~/.grove entirely) sharing the same backing file.
func mountsOf(devices []string) (map[string]string, error) {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return nil, err
	}
	defer f.Close()

	wanted := make(map[string]bool, len(devices))
	for _, d := range devices {
		wanted[d] = true
	}

	mountpoints := make(map[string]string)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) > 1 && wanted[fields[0]] {
			mountpoints[fields[0]] = fields[1]
		}
	}
	return mountpoints, scanner.Err()
}

// warnIfMultiplyMounted checks whether image's backing loop devices are mounted at more than one place at once — the actually dangerous condition, as opposed to merely being *attached* more than once (existingLoopDevices/mountImage's own job). Called from Init's already-mounted fast path, since mountImage's attachment-count check only ever runs on the mount path: if paths.MountPoint is already mounted, Init returns before mountImage is ever reached, so without this a pool reported as "already mounted" could still have a second, completely separate mountpoint (e.g. one created by hand, outside ~/.grove entirely) silently writing through the same backing file. Confirmed live on this machine: a manually-created /mnt/grove-pool mount, set up once while testing and never torn down, sat alongside Grove's own ~/.grove/pool-mount indefinitely, actively corrupting the shared pool.img every time both were mounted — exactly the scenario this exists to catch. Doesn't attempt a fix; the right next step (which mount to keep, which fstab entry to remove) is a decision only the user can make safely.
func warnIfMultiplyMounted(image, ownMountPoint string) error {
	devices, err := existingLoopDevices(image)
	if err != nil {
		return err
	}
	if len(devices) <= 1 {
		return nil
	}

	mountpoints, err := mountsOf(devices)
	if err != nil {
		return err
	}
	if len(mountpoints) <= 1 {
		// More than one attachment, but at most one is actually mounted — the others are dangling, not actively dangerous. Worth a lighter note, not the same severity as a second live mount.
		fmt.Printf("%s %s has %d loop attachments but only one is mounted; the rest are likely leftover from an unclean shutdown. Detach them with `sudo losetup -d <device>` once confirmed idle (`lsof <device>`/`fuser <device>`).\n", output.Warn("Note:"), output.Path(image), len(devices))
		return nil
	}

	var others []string
	for device, mountpoint := range mountpoints {
		if mountpoint != ownMountPoint {
			others = append(others, fmt.Sprintf("%s at %s", device, mountpoint))
		}
	}
	return fmt.Errorf("%s is mounted at %d places at once (%s), in addition to %s — this actively corrupts the pool, since each mount writes through independently with no coordination. Unmount every location except the one you want to keep, remove its /etc/fstab entry so it doesn't come back, then re-run `grove pool init`", image, len(mountpoints), strings.Join(others, ", "), ownMountPoint)
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

// SizeExceedsLimitError reports that a requested pool size exceeds
// maxUsableFraction of free space on the target filesystem. Callers in
// the command layer type-assert for this specifically (rather than
// matching on error text) so they can offer the user MaxGB as a
// concrete, already-computed fallback instead of just failing.
type SizeExceedsLimitError struct {
	RequestedGB uint64
	MaxGB       uint64
	AvailableGB float64
}

func (e *SizeExceedsLimitError) Error() string {
	return fmt.Sprintf("requested %dG exceeds %.0f%% of available space (%.1fG free); choose a smaller --size", e.RequestedGB, maxUsableFraction*100, e.AvailableGB)
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
		return &SizeExceedsLimitError{
			RequestedGB: sizeGB,
			MaxGB:       maxUsableSizeGB(availableBytes),
			AvailableGB: availableGB,
		}
	}
	return nil
}

// maxUsableSizeGB converts availableBytes into the largest whole-GiB
// pool size that still fits within maxUsableFraction of it, rounding
// down so the suggested fallback never itself exceeds the limit it was
// computed from.
func maxUsableSizeGB(availableBytes uint64) uint64 {
	limitGB := (float64(availableBytes) * maxUsableFraction) / (1 << 30)
	return uint64(limitGB)
}

// MaxUsableSizeGB reports the largest whole-GiB pool size that currently
// fits within maxUsableFraction of free space on dir's filesystem. Used
// by the command layer to offer a concrete fallback size up front (e.g.
// in `grove pool init`'s flag help), separately from the
// SizeExceedsLimitError path that reports it after an actual attempt
// fails.
func MaxUsableSizeGB(dir string) (uint64, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(dir, &stat); err != nil {
		return 0, err
	}
	return maxUsableSizeGB(stat.Bavail * uint64(stat.Bsize)), nil
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

// RemountReadWrite attempts to remount an already-mounted pool
// read-write, for the case where a transient issue (not ongoing
// metadata corruption) tripped the read-only flag. This is deliberately
// non-destructive and safe to attempt first: if the underlying
// corruption is still present, btrfs will simply re-trip back to
// read-only on the next write it can't complete, which the caller can
// detect by checking IsReadOnly again rather than trusting this call's
// own success — `mount -o remount,rw` succeeding only means the remount
// syscall itself completed, not that the filesystem stayed writable.
func RemountReadWrite(mountPoint string) error {
	return runSudo("mount", "-o", "remount,rw", mountPoint)
}

// unmountForRecreate unmounts mountPoint for Recreate, tolerating the
// specific failure mode a pool that has just force-remounted read-only
// tends to produce: `umount` reporting the target busy even though no
// process holds an open file or working directory under it (confirmed
// separately via `fuser`/`lsof` showing only "kernel mount", no PIDs).
// That combination means btrfs itself is still settling internally
// right after the read-only trip, not that something external is really
// using the mount — a plain umount can transiently fail here and
// briefly retrying gives the kernel a moment to finish that before
// escalating.
//
// If it's still busy after retrying, falls back to a lazy unmount
// (`umount -l`): detaches the mountpoint from the namespace immediately
// and lets the kernel finish tearing it down once truly idle, once any
// last in-flight I/O (plausible here, since lsof already surfaced an I/O
// error against one subvolume) drains. This is safe specifically because
// Recreate's caller has already confirmed the destructive rebuild with
// the user — a lazy unmount that appears to succeed immediately but
// finishes async is exactly the right tradeoff once the decision to
// discard the pool's contents has already been made, but not something
// to reach for opportunistically elsewhere.
func unmountForRecreate(mountPoint string) error {
	const retries = 3
	const retryDelay = 2 * time.Second

	var lastErr error
	for i := 0; i < retries; i++ {
		if err := runSudo("umount", mountPoint); err == nil {
			return nil
		} else {
			lastErr = err
		}
		time.Sleep(retryDelay)
	}

	fmt.Println(output.Warn("Plain unmount stayed busy after retrying; falling back to a lazy unmount (-l)."))
	if err := runSudo("umount", "-l", mountPoint); err != nil {
		return fmt.Errorf("plain unmount failed (%w) and lazy unmount also failed: %w", lastErr, err)
	}
	return nil
}

// Recreate destroys the pool image entirely and rebuilds it fresh at the
// same paths — the actual fix once RemountReadWrite has been tried and
// the pool immediately re-trips back to read-only, meaning the metadata
// corruption btrfs detected isn't transient. This is the last resort,
// not a repair: it wipes every worktree's reflinked dependency dirs and
// pool-resident container currently in the pool, not just the one
// subvolume that was originally found corrupted. That's an acceptable
// cost only because everything the pool holds is, by Grove's own design,
// a disposable CoW clone — the real data survives in git and in each
// dependency dir's original source outside the pool. Callers (the
// command layer) are responsible for confirming this with the user
// first; Recreate itself performs no confirmation of its own.
//
// Unmounts via unmountForRecreate (tolerating a busy target, since a
// read-only pool the user is trying to recover from may already be in a
// half-torn-down state), removes the fstab entry so a stale reference
// doesn't linger, deletes the image file, then reuses Init to rebuild it
// from scratch at the same size.
//
// Safe to call again immediately after a failed attempt (e.g.
// resolvePoolSize retrying with a smaller size once Init's space check
// rejects the first): the teardown steps all become no-ops against
// already-torn-down state (nothing mounted, no loop device attached, no
// fstab entry, image file already removed) rather than re-failing, so
// the retry lands directly on Init with nothing left to undo.
func Recreate(paths Paths, sizeGB uint64) error {
	if mounted, err := IsMounted(paths.MountPoint); err != nil {
		return err
	} else if mounted {
		if err := unmountForRecreate(paths.MountPoint); err != nil {
			return fmt.Errorf("unmount %s before recreating the pool: %w", paths.MountPoint, err)
		}
	}

	if devices, err := existingLoopDevices(paths.Image); err == nil {
		for _, device := range devices {
			if err := runSudo("losetup", "-d", device); err != nil {
				return fmt.Errorf("detach %s before recreating the pool: %w", device, err)
			}
		}
	}

	if err := removeFstabEntry(paths.MountPoint); err != nil {
		return err
	}

	if err := os.Remove(paths.Image); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove old pool image %s: %w", paths.Image, err)
	}

	return Init(paths, sizeGB)
}

// Usage reports the pool image's total and used bytes, via `df`-style
// statfs against the mount point rather than the image file's own
// (sparse, so meaningless) size on disk. Used both by `grove pool status`
// to warn before a caller hits ENOSPC mid-operation, and by the command
// layer to suggest a sensible default grow amount.
func Usage(mountPoint string) (usedBytes, totalBytes uint64, err error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(mountPoint, &stat); err != nil {
		return 0, 0, err
	}

	totalBytes = stat.Blocks * uint64(stat.Bsize)
	freeBytes := stat.Bfree * uint64(stat.Bsize)
	if freeBytes > totalBytes {
		// Defensive: btrfs's own overcommit/allocation accounting can, in rare
		// cases, make Bfree momentarily exceed Blocks as seen through statfs.
		// Reporting 0 used rather than wrapping to a huge uint64 keeps this a
		// harmless "can't tell right now" instead of a wildly wrong number.
		return 0, totalBytes, nil
	}
	return totalBytes - freeBytes, totalBytes, nil
}

// ErrShrinkTargetTooSmall is returned by Shrink when targetGB doesn't
// leave enough headroom above the pool's current usage. Checked with
// Usage's *used* bytes rather than comparing targetGB against current
// total capacity: shrinking to anything at or above current usage plus
// shrinkHeadroomFraction is what actually determines whether btrfs can
// relocate existing data into the smaller space, not whether the target
// is technically less than today's size.
type ErrShrinkTargetTooSmall struct {
	TargetGB uint64
	UsedGB   float64
	MinGB    uint64
}

func (e *ErrShrinkTargetTooSmall) Error() string {
	return fmt.Sprintf("target size %dG doesn't leave enough room above the pool's current usage (%.1fG used); choose at least %dG", e.TargetGB, e.UsedGB, e.MinGB)
}

// shrinkHeadroomFraction is the fraction of extra room (beyond current
// usage) Shrink insists on keeping. btrfs's own accounting, and the fact
// that a running system may write more data between this check and the
// resize actually happening, mean shrinking to exactly current usage is
// asking for the resize to fail partway through relocating data —
// leaving this margin is what turns "btrfs shrink fails deep inside
// relocation with a cryptic error" into "Grove refuses up front with a
// clear one", the whole point of checking usage before attempting this
// at all.
const shrinkHeadroomFraction = 0.10

// minShrinkTargetGB computes the smallest target size Shrink will accept
// for a pool currently using usedBytes: current usage plus
// shrinkHeadroomFraction of headroom, rounded up to a whole GiB so the
// suggested minimum is never itself too tight.
func minShrinkTargetGB(usedBytes uint64) uint64 {
	minBytes := float64(usedBytes) * (1 + shrinkHeadroomFraction)
	return uint64(minBytes/(1<<30)) + 1
}

// ValidateShrinkTarget checks whether targetGB is a safe shrink target
// for the pool at paths — enough headroom above current usage (see
// ErrShrinkTargetTooSmall), and actually smaller than the pool's current
// size — without touching anything on disk. Exported so the command
// layer can reject an unsafe target before asking the user to confirm a
// destructive-sounding prompt, rather than only finding out after they've
// already said yes. Shrink calls this internally too, so it stays safe
// against any other caller that skips this pre-check.
func ValidateShrinkTarget(paths Paths, targetGB uint64) error {
	mounted, err := IsMounted(paths.MountPoint)
	if err != nil {
		return err
	}
	if !mounted {
		return fmt.Errorf("pool isn't mounted (run `grove pool init` first)")
	}

	usedBytes, totalBytes, err := Usage(paths.MountPoint)
	if err != nil {
		return fmt.Errorf("check current pool usage: %w", err)
	}

	minGB := minShrinkTargetGB(usedBytes)
	if targetGB < minGB {
		return &ErrShrinkTargetTooSmall{TargetGB: targetGB, UsedGB: float64(usedBytes) / (1 << 30), MinGB: minGB}
	}

	currentGB := totalBytes / (1 << 30)
	if targetGB >= currentGB {
		return fmt.Errorf("target size %dG isn't smaller than the pool's current size (%dG); use `grove pool resize +N` to grow instead", targetGB, currentGB)
	}

	return nil
}

// Shrink shrinks the pool to targetGB gigabytes: first shrinks the live
// btrfs filesystem down with `btrfs filesystem resize`, then truncates
// the backing file down to match, then refreshes the loop device
// attached to it (see refreshLoopDeviceSize) so the kernel's own view of
// the file's size matches what's actually on disk. The pool must already
// be mounted.
//
// This is the exact reverse order from Resize's grow path, and
// necessarily so: btrfs needs the device at its *original*, larger size
// while it relocates any data or metadata currently sitting in the
// region being cut off, so shrinking the backing file first would leave
// nowhere for that relocation to happen. Truncating only after btrfs
// confirms it fit into the smaller size is also what keeps a failed
// attempt safe to retry or abandon: if the btrfs resize step fails, this
// returns immediately with the backing file and loop device untouched,
// rather than leaving them shrunk out from under a btrfs filesystem that
// never actually got smaller.
//
// targetGB is checked via ValidateShrinkTarget before anything is
// touched, precisely so a target that's technically smaller than today's
// total but doesn't leave btrfs anywhere to relocate into is rejected
// with a clear message up front instead of failing deep inside the
// btrfs resize itself.
func Shrink(paths Paths, targetGB uint64) error {
	if err := ValidateShrinkTarget(paths, targetGB); err != nil {
		return err
	}

	targetBytes := targetGB << 30

	if err := runSudo("btrfs", "filesystem", "resize", fmt.Sprintf("%d", targetBytes), paths.MountPoint); err != nil {
		return fmt.Errorf("shrink btrfs filesystem (backing file and loop device are untouched — safe to retry or abandon): %w", err)
	}

	if err := runSudo("truncate", "-s", fmt.Sprintf("%dG", targetGB), paths.Image); err != nil {
		return fmt.Errorf("btrfs filesystem was already shrunk to %dG, but truncating the backing file to match failed (re-run `grove pool shrink %d` to retry just this step, or `sudo truncate -s %dG %s` directly): %w", targetGB, targetGB, targetGB, paths.Image, err)
	}

	if err := refreshLoopDeviceSize(paths.Image); err != nil {
		return fmt.Errorf("btrfs filesystem and backing file were already shrunk to reflect this, but the loop device attached to it wasn't told about the new size: %w", err)
	}

	return nil
}

// currentImageSizeBytes returns image's own apparent (not disk/sparse)
// size, via os.Stat, rather than trusting the btrfs filesystem's current
// size (as reported by Usage/statfs) to reflect it. These two can
// legitimately disagree — if a prior resize attempt grew the backing
// file but failed or was interrupted before the loop device/btrfs step
// caught up (see refreshLoopDeviceSize), the file is already larger than
// what btrfs currently sees. Computing the next truncate target from the
// file's real size, rather than from the btrfs-visible size, is what
// keeps a retry after such a partial failure correct instead of
// re-adding the same delta on top of space that's already there.
func currentImageSizeBytes(image string) (uint64, error) {
	info, err := os.Stat(image)
	if err != nil {
		return 0, err
	}
	return uint64(info.Size()), nil
}

// ErrShrinkNotSupported is returned by Resize when deltaGB is 0, since a
// no-op delta isn't a valid grow request. Shrinking has its own
// dedicated path — see Shrink — with a different precondition order and
// its own headroom check, so Resize itself never attempts it.
var ErrShrinkNotSupported = errors.New("shrinking isn't supported via Resize; use Shrink instead")

// Resize grows the pool by deltaGB gigabytes: extends the sparse backing
// file with `truncate`, refreshes the loop device attached to it so the
// kernel picks up the new size (see refreshLoopDeviceSize), then grows
// the live btrfs filesystem to fill it with `btrfs filesystem resize
// max`. The pool must already be mounted — Resize doesn't mount, create,
// or otherwise provision it (see Init for that).
//
// Order matters and is not reversible by swapping it: `btrfs filesystem
// resize` asks the block device for its current size, and a loop device
// only reflects a truncated backing file's new size once explicitly told
// to re-read it — so each step here depends on the one before it having
// actually taken effect first, not just been issued.
//
// deltaGB must be positive; see ErrShrinkNotSupported for why shrinking
// is out of scope. Every step runs via sudo (see runSudo) since
// truncating a file systemd/fstab may reference, reattaching a loop
// device, and resizing a mounted filesystem all need root on most
// systems, the same as every other pool operation that touches the image
// or its mount.
func Resize(paths Paths, deltaGB uint64) error {
	if deltaGB == 0 {
		return ErrShrinkNotSupported
	}

	mounted, err := IsMounted(paths.MountPoint)
	if err != nil {
		return err
	}
	if !mounted {
		return fmt.Errorf("pool isn't mounted (run `grove pool init` first)")
	}

	if err := checkResizeHeadroom(filepath.Dir(paths.Image), deltaGB); err != nil {
		return err
	}

	beforeBytes, err := currentImageSizeBytes(paths.Image)
	if err != nil {
		return fmt.Errorf("check current pool image size: %w", err)
	}

	if err := runSudo("truncate", "-s", fmt.Sprintf("+%dG", deltaGB), paths.Image); err != nil {
		return fmt.Errorf("grow backing file: %w", err)
	}

	afterBytes, sizeErr := currentImageSizeBytes(paths.Image)
	if sizeErr == nil && afterBytes != beforeBytes+(deltaGB<<30) {
		// truncate reported success but the file didn't end up at the expected
		// size — surface this rather than silently proceeding to resize btrfs
		// against a file that isn't the size Resize thinks it is.
		fmt.Printf("%s pool image is now %.1fG (expected %.1fG) — proceeding, but double-check with `grove pool status` after this completes.\n",
			output.Warn("Note:"), float64(afterBytes)/(1<<30), float64(beforeBytes+(deltaGB<<30))/(1<<30))
	}

	if err := refreshLoopDeviceSize(paths.Image); err != nil {
		return fmt.Errorf("backing file was grown to reflect this, but the loop device attached to it wasn't told about the new size (needed before btrfs can see the extra space): %w", err)
	}

	if err := runSudo("btrfs", "filesystem", "resize", "max", paths.MountPoint); err != nil {
		return fmt.Errorf("grow btrfs filesystem (backing file and loop device were already grown to reflect this — re-run `grove pool resize` to retry just this step, or `sudo btrfs filesystem resize max %s` directly): %w", paths.MountPoint, err)
	}

	return nil
}

// refreshLoopDeviceSize tells the kernel to re-read the current size of
// image's backing file into whichever loop device it's attached to, via
// `losetup -c` (LOOP_SET_CAPACITY). A loop device caches the backing
// file's size as of when it was attached — growing the file underneath
// it with truncate doesn't implicitly propagate, so without this step
// `btrfs filesystem resize` (which asks the block device for its size,
// not the file directly) sees the same old size and "resize max" becomes
// a no-op that still reports success. Confirmed live on this machine:
// truncating pool.img from 10G to 15G and then running `btrfs filesystem
// resize max` left the filesystem at exactly 10G, silently.
//
// Requires exactly one loop device attached to image — the same
// precondition mountImage's own attachment-count handling already
// assumes elsewhere in this file, since Resize only ever runs against an
// already-mounted pool.
func refreshLoopDeviceSize(image string) error {
	devices, err := existingLoopDevices(image)
	if err != nil {
		return err
	}
	switch len(devices) {
	case 0:
		return fmt.Errorf("%s has no attached loop device (expected exactly one for an already-mounted pool)", image)
	case 1:
		return runSudo("losetup", "-c", devices[0])
	default:
		return fmt.Errorf("%s is attached to %d loop devices at once (%s); refusing to guess which one to resize — see `grove pool status` and resolve the extra attachment first", image, len(devices), strings.Join(devices, ", "))
	}
}

// checkResizeHeadroom applies the same maxUsableFraction guard Init uses
// for the initial pool size to a resize's delta, against dir's *current*
// free space — growing the pool is still bounded by how much real disk
// is actually available on the host filesystem the image file itself
// lives on, same constraint, just against the additional amount rather
// than the whole requested size.
func checkResizeHeadroom(dir string, deltaGB uint64) error {
	var stat unix.Statfs_t
	if err := unix.Statfs(dir, &stat); err != nil {
		return err
	}

	availableBytes := stat.Bavail * uint64(stat.Bsize)
	requestedBytes := deltaGB << 30
	limit := float64(availableBytes) * maxUsableFraction
	if float64(requestedBytes) > limit {
		availableGB := float64(availableBytes) / (1 << 30)
		return &SizeExceedsLimitError{
			RequestedGB: deltaGB,
			MaxGB:       maxUsableSizeGB(availableBytes),
			AvailableGB: availableGB,
		}
	}
	return nil
}

// removeFstabEntry strips any line referencing mountPoint from
// /etc/fstab, the reverse of ensureFstabEntry, so Recreate doesn't leave
// a dangling fstab entry pointing at an image file that no longer
// exists — systemd would otherwise trip over that on the next boot
// despite `nofail`, and ensureFstabEntry's own "does this line already
// exist" check would then see the stale line and skip writing a fresh
// one for the recreated image.
func removeFstabEntry(mountPoint string) error {
	data, err := os.ReadFile(fstabPath)
	if err != nil {
		return err
	}

	var kept []string
	changed := false
	for _, line := range strings.Split(string(data), "\n") {
		if strings.Contains(line, mountPoint) {
			changed = true
			continue
		}
		kept = append(kept, line)
	}
	if !changed {
		return nil
	}

	cmd := exec.Command("sudo", "tee", fstabPath)
	cmd.Stdin = strings.NewReader(strings.Join(kept, "\n"))
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
