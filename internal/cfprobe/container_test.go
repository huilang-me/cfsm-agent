package cfprobe

import "testing"

func TestCgroupIndicatesContainer(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want bool
	}{
		{
			name: "cgroup v2 docker scope",
			raw:  "0::/system.slice/docker-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef.scope\n",
			want: true,
		},
		{
			name: "cgroup v1 docker cgroupfs",
			raw: "11:memory:/docker/0123456789abcdef\n" +
				"1:name=systemd:/docker/0123456789abcdef\n",
			want: true,
		},
		{
			name: "cgroup v1 docker systemd driver",
			raw:  "4:cpu,cpuacct:/system.slice/docker-abc.scope\n",
			want: true,
		},
		{
			name: "podman rootless",
			raw:  "0::/user.slice/user-1000.slice/user@1000.service/user.slice/libpod-abc123.scope\n",
			want: true,
		},
		{
			name: "containerd cri",
			raw:  "0::/system.slice/cri-containerd-abc123.scope\n",
			want: true,
		},
		{
			name: "crio",
			raw:  "0::/system.slice/crio-abc123.scope\n",
			want: true,
		},
		{
			name: "kubernetes pod",
			raw:  "0::/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod1.slice/cri-containerd-abc.scope\n",
			want: true,
		},
		{
			name: "lxc",
			raw:  "0::/lxc.payload.web\n",
			want: true,
		},
		{
			name: "host systemd unit",
			raw:  "0::/system.slice/cf-probe.service\n",
			want: false,
		},
		{
			name: "host user session",
			raw:  "0::/user.slice/user-1000.slice/session-2.scope\n",
			want: false,
		},
		{
			name: "docker daemon unit",
			raw:  "0::/system.slice/docker.service\n",
			want: false,
		},
		{
			name: "containerd daemon unit",
			raw:  "0::/system.slice/containerd.service\n",
			want: false,
		},
		{
			name: "root cgroup",
			raw:  "0::/\n",
			want: false,
		},
		{
			name: "empty",
			raw:  "",
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cgroupIndicatesContainer(tt.raw); got != tt.want {
				t.Fatalf("cgroupIndicatesContainer(%q) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}

// The container branch must stay fail-closed: only a host-side caller may skip
// a container-hosted instance, so root and non-root installs inside the same
// container keep replacing each other.
func TestSkipForeignProbeInstance(t *testing.T) {
	tests := []struct {
		name              string
		selfInContainer   bool
		targetInContainer bool
		want              bool
	}{
		{name: "host skips container", selfInContainer: false, targetInContainer: true, want: true},
		{name: "host blocks host", selfInContainer: false, targetInContainer: false, want: false},
		{name: "container blocks other container", selfInContainer: true, targetInContainer: true, want: false},
		{name: "container blocks host", selfInContainer: true, targetInContainer: false, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := skipForeignProbeInstance(tt.selfInContainer, tt.targetInContainer); got != tt.want {
				t.Fatalf("skipForeignProbeInstance(%v, %v) = %v, want %v",
					tt.selfInContainer, tt.targetInContainer, got, tt.want)
			}
		})
	}
}
