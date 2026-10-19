package executor

import (
	"context"
	"errors"
	"testing"
)

func TestIsSandbox(t *testing.T) {
	if !IsSandbox(&Sandbox{}) {
		t.Error("IsSandbox(*Sandbox) = false, want true")
	}
	if IsSandbox(NewLocal()) {
		t.Error("IsSandbox(*Local) = true, want false")
	}
	if IsSandbox(nil) {
		t.Error("IsSandbox(nil) = true, want false")
	}
}

func TestIsSandboxThroughTranslate(t *testing.T) {
	sbx := &Translate{Inner: &Sandbox{}, HostRoot: "/h", SandboxRoot: "/workspace"}
	if !IsSandbox(sbx) {
		t.Error("IsSandbox(Translate{Sandbox}) = false, want true (delegation)")
	}
	loc := &Translate{Inner: NewLocal(), HostRoot: "/h", SandboxRoot: "/workspace"}
	if IsSandbox(loc) {
		t.Error("IsSandbox(Translate{Local}) = true, want false (delegation)")
	}
	if !sbx.IsSandbox() || loc.IsSandbox() {
		t.Error("Translate.IsSandbox() delegation broken")
	}
}

func TestErrSandboxUnsupported(t *testing.T) {
	if !errors.Is(ErrSandboxUnsupported, ErrSandboxUnsupported) {
		t.Fatal("unreachable")
	}
	_ = context.Background
}
