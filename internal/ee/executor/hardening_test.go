package executor

import "testing"

// TestHardeningSpecFull pins the HardeningFull profile: every restriction
// applies, capabilities drop to nothing, and the pid count is capped.
func TestHardeningSpecFull(t *testing.T) {
	spec := hardeningSpecFor(HardeningFull)

	if len(spec.CapDrop) != 1 || spec.CapDrop[0] != "ALL" {
		t.Errorf("full CapDrop = %v, want [ALL]", spec.CapDrop)
	}
	if !containsOpt(spec.SecurityOpt, "no-new-privileges") {
		t.Errorf("full SecurityOpt = %v, want no-new-privileges", spec.SecurityOpt)
	}
	if !spec.ReadonlyRootfs {
		t.Error("full ReadonlyRootfs = false, want true")
	}
	if got, ok := spec.Tmpfs["/tmp"]; !ok || got != "" {
		t.Errorf("full Tmpfs[/tmp] = %q (present=%v), want \"\" (present)", got, ok)
	}
	if spec.PidsLimit != 512 {
		t.Errorf("full PidsLimit = %d, want 512", spec.PidsLimit)
	}
}

// TestHardeningSpecMinimal pins the HardeningMinimal profile: the
// filesystem-root restrictions (read-only rootfs, its tmpfs) and
// no-new-privileges are off, while capability dropping and the pid cap
// stay — minimal relaxes, it never opens the process floodgates.
func TestHardeningSpecMinimal(t *testing.T) {
	spec := hardeningSpecFor(HardeningMinimal)

	if len(spec.CapDrop) != 1 || spec.CapDrop[0] != "ALL" {
		t.Errorf("minimal CapDrop = %v, want [ALL]", spec.CapDrop)
	}
	if containsOpt(spec.SecurityOpt, "no-new-privileges") {
		t.Errorf("minimal SecurityOpt = %v, want no no-new-privileges", spec.SecurityOpt)
	}
	if spec.ReadonlyRootfs {
		t.Error("minimal ReadonlyRootfs = true, want false")
	}
	if len(spec.Tmpfs) != 0 {
		t.Errorf("minimal Tmpfs = %v, want none", spec.Tmpfs)
	}
	if spec.PidsLimit != 512 {
		t.Errorf("minimal PidsLimit = %d, want 512", spec.PidsLimit)
	}
}

// TestHardeningSpecNeverUnconfinesSeccomp guards the third rail: neither
// level may disable the seccomp profile (engine default stays active).
func TestHardeningSpecNeverUnconfinesSeccomp(t *testing.T) {
	for _, level := range []Hardening{HardeningFull, HardeningMinimal} {
		if containsOpt(hardeningSpecFor(level).SecurityOpt, "seccomp=unconfined") {
			t.Errorf("hardening %d sets seccomp=unconfined", level)
		}
	}
}

func containsOpt(opts []string, want string) bool {
	for _, o := range opts {
		if o == want {
			return true
		}
	}
	return false
}
