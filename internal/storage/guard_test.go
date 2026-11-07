package storage

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
)

func TestThresholdsAndBoundaries(t *testing.T) {
	report := Report{Backing: Capacity{Path: "/backing", UsedPercent: 85, FreeBytes: 1024}, WarnPercent: 85, BlockPercent: 90, CriticalPercent: 95}
	if len(report.Warnings()) != 1 {
		t.Fatal("expected warning at 85%")
	}
	if err := report.ErrorForMutation(false, false, false); err != nil {
		t.Fatalf("ordinary mutation blocked at 85%%: %v", err)
	}
	report.Backing.UsedPercent = 90
	if err := report.ErrorForMutation(true, false, false); err == nil {
		t.Fatal("expected storage-intensive block at 90%")
	}
	report.Backing.UsedPercent = 95
	if err := report.ErrorForMutation(false, false, false); err == nil {
		t.Fatal("expected all mutations blocked at 95%")
	}
}

func TestInodeAndZeroCapacityProtection(t *testing.T) {
	report := Report{Backing: Capacity{Path: "/backing", UsedPercent: 20, InodeUsedPercent: 92, FreeBytes: 1024, FreeInodes: 1}, WarnPercent: 85, BlockPercent: 90, CriticalPercent: 95}
	if err := report.ErrorForMutation(true, false, false); err == nil {
		t.Fatal("expected inode pressure block")
	}
	report.Backing.InodeUsedPercent = 20
	report.Backing.FreeBytes = 0
	if err := report.ErrorForMutation(false, false, false); err == nil {
		t.Fatal("expected zero free bytes to fail closed")
	}
	report.Backing.FreeBytes = 1024
	report.Backing.TotalInodes = 100
	report.Backing.FreeInodes = 0
	if err := report.ErrorForMutation(false, false, false); err == nil {
		t.Fatal("expected zero free inodes to fail closed")
	}
}

func TestReadOnlyPoolAndBacking(t *testing.T) {
	pool := Capacity{Path: "/pool", ReadOnly: true}
	report := Report{Backing: Capacity{Path: "/backing"}, Pool: &pool, WarnPercent: 85, BlockPercent: 90, CriticalPercent: 95}
	if err := report.ErrorForMutation(true, false, false); err == nil {
		t.Fatal("expected read-only pool block")
	}
	if err := report.ErrorForMutation(false, true, true); err != nil {
		t.Fatalf("pool repair blocked: %v", err)
	}
	report.Backing.ReadOnly = true
	if err := report.ErrorForMutation(false, true, true); err == nil {
		t.Fatal("read-only backing filesystem must block recovery writes")
	}
}

func TestConcurrentMutationLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "guard.lock")
	first, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()
	var wait sync.WaitGroup
	for range 12 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if lock, err := Acquire(path); err == nil {
				lock.Release()
				t.Error("concurrent mutation bypassed active lock")
			}
		}()
	}
	wait.Wait()
}

func TestMountReadOnlyDetection(t *testing.T) {
	mounts := []byte("/dev/root / ext4 rw,relatime 0 0\n/dev/loop0 /mnt/grove btrfs ro,relatime 0 0\n")
	readOnly, err := mountReadOnlyFrom("/mnt/grove/worktrees/a", mounts)
	if err != nil || !readOnly {
		t.Fatalf("expected read-only mount, got %v, %v", readOnly, err)
	}
	mounts = append(mounts, []byte("/dev/loop1 /mnt/grove/worktrees btrfs rw,relatime 0 0\n")...)
	readOnly, err = mountReadOnlyFrom("/mnt/grove/worktrees/a", mounts)
	if err != nil || readOnly {
		t.Fatalf("expected longest matching mount to be read-write, got %v, %v", readOnly, err)
	}
	if _, err := mountReadOnlyFrom("/unmounted/path", nil); err == nil {
		t.Fatal("expected missing mount entry to fail closed")
	}
}

func TestInspectCapacityAndStorageErrors(t *testing.T) {
	report, err := Inspect(t.TempDir(), "", false)
	if err != nil {
		t.Fatal(err)
	}
	if report.Backing.TotalBytes == 0 || report.Backing.FreeBytes == 0 {
		t.Fatalf("unexpected capacity: %+v", report.Backing)
	}
	if !IsStorageError(errors.Join(os.ErrInvalid, syscall.ENOSPC)) || !IsStorageError(syscall.EROFS) {
		t.Fatal("ENOSPC and EROFS must be detected")
	}
	if IsStorageError(os.ErrPermission) {
		t.Fatal("unrelated error classified as storage failure")
	}
}

func TestMutationLockCanBeReacquired(t *testing.T) {
	path := filepath.Join(t.TempDir(), "guard.lock")
	first, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	second, err := Acquire(path)
	if err != nil {
		t.Fatalf("lock remained held: %v", err)
	}
	second.Release()
}

func TestThresholdConfiguration(t *testing.T) {
	t.Setenv("GROVE_STORAGE_WARN_PERCENT", "80")
	value, err := threshold("GROVE_STORAGE_WARN_PERCENT", defaultWarnPercent)
	if err != nil || value != 80 {
		t.Fatalf("threshold = %d, %v", value, err)
	}
	t.Setenv("GROVE_STORAGE_WARN_PERCENT", "101")
	if _, err := threshold("GROVE_STORAGE_WARN_PERCENT", defaultWarnPercent); err == nil {
		t.Fatal("expected invalid threshold error")
	}
}
