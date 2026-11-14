//go:build !windows

package cfprobe

import (
	"path/filepath"
	"runtime"
	"testing"
)

func TestPathsForInstanceDefaultUnchanged(t *testing.T) {
	system := systemDefaultPaths()
	named := systemDefaultPathsForInstance("")
	if named != system {
		t.Fatalf("systemDefaultPathsForInstance(\"\") = %+v, want %+v", named, system)
	}

	home := t.TempDir()
	user := userPaths("alice", 501, home)
	named = userPathsForInstance("", "alice", 501, home)
	if named != user {
		t.Fatalf("userPathsForInstance(\"\") = %+v, want %+v", named, user)
	}
}

func TestPathsForInstanceNamedUser(t *testing.T) {
	home := t.TempDir()
	paths := userPathsForInstance("eth1", "alice", 501, home)

	if paths.Instance != "eth1" {
		t.Fatalf("Instance = %q", paths.Instance)
	}
	if paths.ServiceName != "cf-probe-eth1" {
		t.Fatalf("ServiceName = %q", paths.ServiceName)
	}
	if want := filepath.Join(home, ".cf-probe-eth1"); paths.ConfigDir != want {
		t.Fatalf("ConfigDir = %q, want %q", paths.ConfigDir, want)
	}
	if want := filepath.Join(home, ".cf-probe-eth1", "bin", "cf-probe"); paths.BinaryFile != want {
		t.Fatalf("BinaryFile = %q, want %q", paths.BinaryFile, want)
	}
	if want := filepath.Join(home, ".cf-probe-eth1", "cf-probe-eth1.pid"); paths.PIDFile != want {
		t.Fatalf("PIDFile = %q, want %q", paths.PIDFile, want)
	}
	if want := filepath.Join(home, ".cf-probe-eth1", "cf-probe-eth1.log"); paths.LogFile != want {
		t.Fatalf("LogFile = %q, want %q", paths.LogFile, want)
	}
	if want := filepath.Join(home, ".config", "systemd", "user", "cf-probe-eth1.service"); paths.ServiceFile != want {
		t.Fatalf("ServiceFile = %q, want %q", paths.ServiceFile, want)
	}
	if paths.LaunchdLabel != "com.cfsm.cf-probe-eth1" {
		t.Fatalf("LaunchdLabel = %q", paths.LaunchdLabel)
	}
	if want := filepath.Join(home, "Library", "LaunchAgents", "com.cfsm.cf-probe-eth1.plist"); paths.LaunchdUserFile != want {
		t.Fatalf("LaunchdUserFile = %q, want %q", paths.LaunchdUserFile, want)
	}
}

func TestPathsForInstanceNamedSystem(t *testing.T) {
	paths := systemDefaultPathsForInstance("eth1")

	if paths.Instance != "eth1" || paths.ServiceName != "cf-probe-eth1" {
		t.Fatalf("Instance/ServiceName = %q/%q", paths.Instance, paths.ServiceName)
	}
	if paths.OldTrafficFile != "" {
		t.Fatalf("OldTrafficFile = %q, want empty for named instance", paths.OldTrafficFile)
	}
	if want := filepath.Join("/var/log", "cf-probe-eth1.log"); paths.LogFile != want {
		t.Fatalf("LogFile = %q, want %q", paths.LogFile, want)
	}
	if want := filepath.Join("/etc/systemd/system", "cf-probe-eth1.service"); paths.ServiceFile != want {
		t.Fatalf("ServiceFile = %q, want %q", paths.ServiceFile, want)
	}
	if paths.LaunchdLabel != "com.cfsm.cf-probe-eth1" {
		t.Fatalf("LaunchdLabel = %q", paths.LaunchdLabel)
	}
	if runtime.GOOS == "darwin" {
		if want := filepath.Join("/usr/local/etc", "cf-probe-eth1"); paths.ConfigDir != want {
			t.Fatalf("ConfigDir = %q, want %q", paths.ConfigDir, want)
		}
	} else if want := filepath.Join("/etc/config", "cf-probe-eth1"); paths.ConfigDir != want {
		t.Fatalf("ConfigDir = %q, want %q", paths.ConfigDir, want)
	}
}
