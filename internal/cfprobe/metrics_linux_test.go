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
