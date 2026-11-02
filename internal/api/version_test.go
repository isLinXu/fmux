package api

import "testing"

func TestResolveVersionPrefersLinkerStamp(t *testing.T) {
	// A stamped version must win outright: goreleaser and the Makefile are
	// the source of truth for released builds.
	if got := resolveVersion("0.1.1"); got != "0.1.1" {
		t.Fatalf("stamped version was overridden: got %q, want 0.1.1", got)
	}
}

func TestResolveVersionFallsBackWhenUnstamped(t *testing.T) {
	// Under `go test` the main module version is "(devel)", so the fallback
	// must hand back the placeholder rather than something like "(devel)".
	for _, in := range []string{"0.0.0-dev", ""} {
		got := resolveVersion(in)
		if got == "(devel)" {
			t.Fatalf("resolveVersion(%q) leaked the toolchain placeholder", in)
		}
		if got != "0.0.0-dev" {
			t.Fatalf("resolveVersion(%q) = %q, want 0.0.0-dev", in, got)
		}
	}
}

func TestVersionIsNeverEmpty(t *testing.T) {
	// Version is surfaced by `fmux version`, the health payload and the
	// startup log line, so an empty value would produce "fmux " output.
	if Version == "" {
		t.Fatal("Version must never be empty")
	}
}
