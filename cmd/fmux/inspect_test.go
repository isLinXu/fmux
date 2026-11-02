package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/isLinXu/fmux/internal/config"
)

// fakeProcesses serves a /admin/processes payload listing the given processes.
func fakeProcesses(t *testing.T, procs []map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/processes" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"processes": procs})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func findByPrefix(checks []doctorCheck, prefix string) *doctorCheck {
	for i := range checks {
		if strings.HasPrefix(checks[i].name, prefix) {
			return &checks[i]
		}
	}
	return nil
}

// TestProbeResidencyFlagsAWrongEstimate is the point of the command: a
// memory_mb that disagrees with the process must be reported with the number
// that should replace it, otherwise the budget silently stops meaning anything.
func TestProbeResidencyFlagsAWrongEstimate(t *testing.T) {
	srv := fakeProcesses(t, []map[string]any{
		{"name": "mlx", "state": "running", "pid": os.Getpid()},
	})

	cfg := &config.Config{
		Listen: strings.TrimPrefix(srv.URL, "http://"),
		Backends: []config.Backend{
			// The test binary holds tens of MB; claiming 1 MB is a gross
			// underestimate, exactly like a hand-written guess usually is.
			{Name: "mlx", MemoryMB: 1},
		},
	}

	chk := findByPrefix(probeResidency(context.Background(), cfg), "memory:mlx")
	if chk == nil {
		t.Fatal("no memory check was produced for the running backend")
	}
	if chk.status != "warn" {
		t.Fatalf("status=%q, want warn (1 MB configured against a real process)", chk.status)
	}
	if !strings.Contains(chk.detail, "set memory_mb:") {
		t.Fatalf("detail should tell the user what to set, got %q", chk.detail)
	}
}

// TestProbeResidencyAcceptsAGoodEstimate guards against the command crying
// wolf: an estimate within tolerance has to come back ok, or nobody will act
// on it when it does warn.
func TestProbeResidencyAcceptsAGoodEstimate(t *testing.T) {
	pid := os.Getpid()
	actual, err := rssMB(pid)
	if err != nil {
		t.Skipf("cannot read own rss: %v", err)
	}

	srv := fakeProcesses(t, []map[string]any{
		{"name": "mlx", "state": "running", "pid": pid},
	})
	cfg := &config.Config{
		Listen:   strings.TrimPrefix(srv.URL, "http://"),
		Backends: []config.Backend{{Name: "mlx", MemoryMB: actual}},
	}

	chk := findByPrefix(probeResidency(context.Background(), cfg), "memory:mlx")
	if chk == nil {
		t.Fatal("no memory check was produced")
	}
	if chk.status != "ok" {
		t.Fatalf("status=%q for an accurate estimate, want ok (detail: %s)", chk.status, chk.detail)
	}
}

// TestProbeResidencyReportsUnsetMemory covers the case that makes the budget
// useless in a different way: a local backend with no memory_mb at all.
func TestProbeResidencyReportsUnsetMemory(t *testing.T) {
	srv := fakeProcesses(t, []map[string]any{
		{"name": "llamacpp", "state": "running", "pid": os.Getpid()},
	})
	cfg := &config.Config{
		Listen:   strings.TrimPrefix(srv.URL, "http://"),
		Backends: []config.Backend{{Name: "llamacpp"}},
	}

	chk := findByPrefix(probeResidency(context.Background(), cfg), "memory:llamacpp")
	if chk == nil {
		t.Fatal("no memory check was produced")
	}
	if chk.status != "warn" || !strings.Contains(chk.detail, "memory_mb is unset") {
		t.Fatalf("status=%q detail=%q, want a warn about the unset budget", chk.status, chk.detail)
	}
}

// TestProbeResidencySkipsStoppedProcesses makes sure a stopped backend is not
// measured: its PID is gone and reading it would report a false mismatch.
func TestProbeResidencySkipsStoppedProcesses(t *testing.T) {
	srv := fakeProcesses(t, []map[string]any{
		{"name": "mlx", "state": "stopped", "pid": 0},
	})
	cfg := &config.Config{
		Listen:   strings.TrimPrefix(srv.URL, "http://"),
		Backends: []config.Backend{{Name: "mlx", MemoryMB: 5120}},
	}

	checks := probeResidency(context.Background(), cfg)
	if chk := findByPrefix(checks, "memory:mlx"); chk != nil {
		t.Fatalf("a stopped process should not be measured, got %+v", *chk)
	}
	probe := findByPrefix(checks, "probe")
	if probe == nil || !strings.Contains(probe.detail, "no supervised backend is running") {
		t.Fatalf("expected a hint about starting a backend, got %+v", checks)
	}
}

// TestProbeResidencyWithoutGateway tells the user what to do instead of
// failing opaquely when the gateway is not up.
func TestProbeResidencyWithoutGateway(t *testing.T) {
	srv := fakeProcesses(t, nil)
	listen := strings.TrimPrefix(srv.URL, "http://")
	srv.Close() // now nothing is listening there

	cfg := &config.Config{Listen: listen}
	checks := probeResidency(context.Background(), cfg)

	probe := findByPrefix(checks, "probe")
	if probe == nil {
		t.Fatalf("expected a probe result, got %+v", checks)
	}
	if probe.status != "warn" || !strings.Contains(probe.detail, "gateway is not running") {
		t.Fatalf("status=%q detail=%q, want a hint to start the gateway", probe.status, probe.detail)
	}
}

func TestRSSMBReadsOwnProcess(t *testing.T) {
	mb, err := rssMB(os.Getpid())
	if err != nil {
		t.Fatalf("rssMB: %v", err)
	}
	// A Go test binary is at least a few MB; anything under 1 means the unit
	// conversion is wrong (ps reports KB).
	if mb < 1 || mb > 100_000 {
		t.Fatalf("own rss reported as %d MB, which is implausible", mb)
	}
	fmt.Fprintf(os.Stderr, "test process rss: %d MB\n", mb)
}

// TestRSSMBTracksAllocations checks that the number is not merely plausible
// but actually follows the process's resident memory. A wrong struct offset
// would still return a stable, believable figure.
func TestRSSMBTracksAllocations(t *testing.T) {
	before, err := rssMB(os.Getpid())
	if err != nil {
		t.Skipf("cannot read own rss: %v", err)
	}

	const allocMB = 64
	buf := make([]byte, allocMB<<20)
	for i := 0; i < len(buf); i += 4096 {
		buf[i] = 1 // touch every page so it becomes resident
	}

	after, err := rssMB(os.Getpid())
	if err != nil {
		t.Fatalf("rssMB after alloc: %v", err)
	}
	runtime.KeepAlive(buf)

	// Allow slack: the runtime may release or reuse pages, but 64 MB of
	// touched memory must show up as at least 48 MB.
	if grew := after - before; grew < allocMB*3/4 {
		t.Fatalf("allocated %d MB but rss only grew by %d MB (%d -> %d)",
			allocMB, grew, before, after)
	}
}
