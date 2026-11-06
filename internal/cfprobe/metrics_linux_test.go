//go:build linux

package cfprobe

import (
	"syscall"
	"testing"
)

func TestLinuxMountFsidKey(t *testing.T) {
	var st syscall.Statfs_t
	// Unset f_fsid must return "" so callers fall back to the device string
	// rather than collapsing unrelated mounts.
	if got := linuxMountFsidKey(&st); got != "" {
		t.Fatalf("zero fsid: got %q, want empty", got)
	}
	st.Fsid.X__val[0] = 1
	if got := linuxMountFsidKey(&st); got == "" {
		t.Fatal("non-zero fsid must produce a key")
	}

	// Same fsid -> same key (this is what folds overlay "/" and a bind "/data"
	// that share one underlying filesystem).
	st2 := st
	if linuxMountFsidKey(&st) != linuxMountFsidKey(&st2) {
		t.Fatal("identical fsid must yield identical key")
	}
	st2.Fsid.X__val[1] = 99
	if linuxMountFsidKey(&st) == linuxMountFsidKey(&st2) {
		t.Fatal("different fsid must yield different key")
	}
}

func TestLinuxMountDedupKey(t *testing.T) {
	var st syscall.Statfs_t
	st.Fsid.X__val[0] = 7
	st.Fsid.X__val[1] = 11

	// Non-zfs filesystems must keep the exact key they had before, otherwise
	// dedup results on every other host would change.
	for _, fsType := range []string{"ext4", "xfs", "btrfs", "overlay", "trimafs", "vfat"} {
		if got, want := linuxMountDedupKey(fsType, "/dev/sda2", &st), linuxMountFsidKey(&st); got != want {
			t.Fatalf("%s: key changed, got %q want %q", fsType, got, want)
		}
	}

	// ZFS: datasets of one pool fold together, different pools never do, and
	// the f_fsid is ignored because it differs per dataset.
	if got, want := linuxMountDedupKey("zfs", "default", &st), "zfs:default"; got != want {
		t.Fatalf("zfs pool key: got %q want %q", got, want)
	}
	if linuxMountDedupKey("zfs", "default", &st) != linuxMountDedupKey("ZFS", "default", &st) {
		t.Fatal("zfs key must be case-insensitive on the fs type")
	}
	if linuxMountDedupKey("zfs", "default", &st) == linuxMountDedupKey("zfs", "tank", &st) {
		t.Fatal("different pools must not fold")
	}
	st2 := st
	st2.Fsid.X__val[1] = 99
	if linuxMountDedupKey("zfs", "default", &st) != linuxMountDedupKey("zfs", "default", &st2) {
		t.Fatal("zfs key must not depend on f_fsid")
	}

	// Unknown source falls back to the old behaviour instead of collapsing
	// every anonymous zfs mount into one entry.
	if got, want := linuxMountDedupKey("zfs", "", &st), linuxMountFsidKey(&st); got != want {
		t.Fatalf("empty zfs source must fall back, got %q want %q", got, want)
	}
}

func TestDiskEntryPreferred(t *testing.T) {
	realDev := diskUsageEntry{total: 100, used: 50, device: DiskDeviceRef{Major: 8}, hasDevice: true}
	overlay := diskUsageEntry{total: 100, used: 50, device: DiskDeviceRef{Major: 0}}

	// Larger capacity always wins (quota handling).
	if !diskEntryPreferred(diskUsageEntry{total: 200}, realDev) {
		t.Fatal("larger total should be preferred")
	}
	if diskEntryPreferred(realDev, diskUsageEntry{total: 200}) {
		t.Fatal("smaller total should not be preferred")
	}

	// On a tie, prefer the entry with a real block device (non-zero major) over
	// an anonymous overlay device so DiskDevices keeps a usable major/minor.
	if !diskEntryPreferred(realDev, overlay) {
		t.Fatal("real device should win a tie over overlay (major 0)")
	}
	if diskEntryPreferred(overlay, realDev) {
		t.Fatal("overlay should not replace a real device on a tie")
	}
}

func TestDistinctBlockDevices(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"/dev/sda1", "/dev/sdb1", true},  // two different real disks: never merge
		{"/dev/sda1", "/dev/sda1", false}, // same device (bind mount): may merge
		{"overlay", "/dev/sda1", false},   // container root folds into its backing disk
		{"tmpfs", "/dev/sda1", false},     // non-/dev source is not a real disk
		{"/data", "/backup", false},       // non-/dev sources never trip the guard
	}
	for _, c := range cases {
		if got := distinctBlockDevices(c.a, c.b); got != c.want {
			t.Fatalf("distinctBlockDevices(%q,%q)=%v want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestIncludeLinuxMountFnOS(t *testing.T) {
	// fnOS (飞牛) mounts its union/view filesystem at /fs while the real volume is
	// mounted at /vol1 (btrfs on LVM). Both report the same capacity but carry a
	// different f_fsid, so the trimafs view must not be counted on top of it.
	if includeLinuxMount("trimafs", "/fs", "trimafs", "rw,relatime,trimacl,ino_mode=32") {
		t.Fatal("trimafs view must not add extra capacity on top of the real volume")
	}
	if !includeLinuxMount("/dev/mapper/trim_x-0", "/vol1", "btrfs", "rw,relatime,ssd") {
		t.Fatal("real btrfs volume must still be counted")
	}
	if !includeLinuxMount("/dev/nvme0n1p2", "/", "ext4", "rw,relatime") {
		t.Fatal("root filesystem must still be counted")
	}
}
