package executor

// This file is the pure-logic half of the docker driver's security posture:
// it describes the hardening profile in docker-free terms (no moby imports,
// no build tags) so plain `go test ./...` can pin it, and the dockerlab-tagged
// driver maps it onto HostConfig verbatim.

// hardeningSpec is the dockerless description of a container's security
// posture. Field-by-field mapping onto docker's HostConfig happens in the
// dockerlab-tagged driver; keeping it docker-free here means the profile is
// testable without a daemon.
type hardeningSpec struct {
	// CapDrop lists Linux capabilities to drop; both levels drop ALL.
	CapDrop []string
	// SecurityOpt carries extra daemon security options. Full adds
	// "no-new-privileges"; minimal leaves it off. Neither level ever adds
	// "seccomp=unconfined" — the engine's default seccomp profile stays
	// active everywhere.
	SecurityOpt []string
	// ReadonlyRootfs mounts the container root filesystem read-only.
	ReadonlyRootfs bool
	// Tmpfs lists writable tmpfs mounts (only needed with ReadonlyRootfs).
	Tmpfs map[string]string
	// PidsLimit caps the container's process count (fork-bomb net).
	PidsLimit int64
}

// hardeningSpecFor returns the hardening profile for a level.
//
// Full (the zero value, so an unset CreateSpec fails closed): drop every
// capability, forbid privilege escalation, read-only rootfs with /tmp the
// only writable scratch, pid cap 512. Masked paths (the /proc/* holes the
// engine masks by default) are never widened at either level — the driver
// simply leaves the engine defaults in place.
//
// Minimal: keeps CapDrop=["ALL"], the default seccomp profile, and the pid
// cap, but drops no-new-privileges, the read-only rootfs, and its tmpfs for
// workloads that cannot run fully hardened.
func hardeningSpecFor(level Hardening) hardeningSpec {
	spec := hardeningSpec{
		CapDrop:   []string{"ALL"},
		PidsLimit: 512,
	}
	if level == HardeningFull {
		spec.SecurityOpt = []string{"no-new-privileges"}
		spec.ReadonlyRootfs = true
		spec.Tmpfs = map[string]string{"/tmp": ""}
	}
	return spec
}
