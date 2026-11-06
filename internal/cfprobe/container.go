package cfprobe

import "strings"

// skipForeignProbeInstance reports whether a running probe instance must be
// ignored because it lives in a container while the caller runs on the host.
// Container instances are invisible to the path-based conflict checks, so
// blocking on them would stop every host install on a machine that also runs
// the Docker image, and killing them would bounce the container. Everything
// else, including the case where either side cannot be classified, keeps the
// fail-closed behaviour: a root install and a non-root install on the same host
// still exclude each other.
func skipForeignProbeInstance(selfInContainer, targetInContainer bool) bool {
	return !selfInContainer && targetInContainer
}

// cgroupIndicatesContainer matches the cgroup paths used by the common
// container runtimes. Host units such as docker.service deliberately do not
// match: the markers require the runtime's id or scope suffix.
func cgroupIndicatesContainer(raw string) bool {
	markers := []string{
		"docker-", "docker/",
		"libpod-", "libpod/",
		"cri-containerd-", "crio-",
		"kubepods",
		"lxc.payload.", "/lxc/",
	}
	for _, line := range strings.Split(raw, "\n") {
		path := line
		if _, rest, ok := strings.Cut(line, ":"); ok {
			_, path, _ = strings.Cut(rest, ":")
		}
		path = strings.ToLower(strings.TrimSpace(path))
		if path == "" {
			continue
		}
		for _, marker := range markers {
			if strings.Contains(path, marker) {
				return true
			}
		}
	}
	return false
}
