package supervisor

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"testing"
	"time"
)

// freePort reserves and releases a TCP port so a child process can bind it.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

func requirePython(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
}

// TestEnsureStartsAndStops drives a real child process: python's http.server
// answers the health probe, and Stop must actually reap it.
func TestEnsureStartsAndStops(t *testing.T) {
	requirePython(t)
	port := freePort(t)
	url := fmt.Sprintf("http://127.0.0.1:%d/", port)

	s := New(nil)
	s.Register(Spec{
		Name: "fake",
		Command: []string{
			"python3", "-m", "http.server", fmt.Sprint(port), "--bind", "127.0.0.1",
		},
		HealthURL:      url,
		StartupTimeout: 15 * time.Second,
		StopTimeout:    5 * time.Second,
		LogPath:        t.TempDir() + "/fake.log",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if err := s.Ensure(ctx, "fake"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	first := s.Status()[0]
	if first.State != StateRunning || first.PID == 0 {
		t.Fatalf("status = %+v, want running with pid", first)
	}

	// A second Ensure must reuse the running process, not spawn another.
	if err := s.Ensure(ctx, "fake"); err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	if second := s.Status()[0]; second.PID != first.PID {
		t.Fatalf("pid changed on second ensure: %d -> %d", first.PID, second.PID)
	}

	if err := s.Stop("fake"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if st := s.Status()[0]; st.State != StateStopped || st.PID != 0 {
		t.Fatalf("after stop: %+v", st)
	}
}

// TestEnsureReportsStartupFailure covers a command that exits immediately.
func TestEnsureReportsStartupFailure(t *testing.T) {
	s := New(nil)
	s.Register(Spec{
		Name:           "broken",
		Command:        []string{"/bin/sh", "-c", "exit 3"},
		HealthURL:      "http://127.0.0.1:1/",
		StartupTimeout: 2 * time.Second,
		StopTimeout:    time.Second,
		LogPath:        t.TempDir() + "/broken.log",
	})
	err := s.Ensure(context.Background(), "broken")
	if err == nil {
		t.Fatal("expected an error for a command that exits during startup")
	}
	if st := s.Status()[0]; st.State != StateFailed {
		t.Fatalf("state = %q, want failed", st.State)
	}
}

// TestEnsureWithoutCommandIsNoop covers externally managed backends such as a
// hand-started Ollama: the supervisor must not try to launch anything.
func TestEnsureWithoutCommandIsNoop(t *testing.T) {
	s := New(nil)
	s.Register(Spec{Name: "external"})
	if err := s.Ensure(context.Background(), "external"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
}

func TestEnsureUnknownName(t *testing.T) {
	s := New(nil)
	if err := s.Ensure(context.Background(), "nope"); err == nil {
		t.Fatal("expected an error for an unregistered name")
	}
}
