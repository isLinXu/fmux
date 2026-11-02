package supervisor

import (
	"context"
	"io"
	"log/slog"
	"os/exec"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func quietSupervisorLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// haveSleep reports whether /bin/sleep exists, so the process tests can skip
// on a system without it instead of failing for the wrong reason.
func haveSleep(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("sleep not available")
	}
	return path
}

func TestConcurrentEnterReleaseBalances(t *testing.T) {
	s := New(quietSupervisorLog())
	s.Register(Spec{Name: "p", Command: []string{haveSleep(t), "60"}, StopTimeout: 2 * time.Second})

	const goroutines, rounds = 32, 40
	var wg sync.WaitGroup

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < rounds; j++ {
				release := s.Enter("p")
				_ = s.Inflight("p")
				_ = s.Status()
				release()
			}
		}()
	}
	wg.Wait()

	if got := s.Inflight("p"); got != 0 {
		t.Fatalf("inflight=%d after every Enter had a matching release, want 0", got)
	}
}

// TestEnterReleaseIsIdempotent checks the once-guard around release. A double
// release that decrements twice would let the reaper stop a process that still
// has a live request on it.
func TestEnterReleaseIsIdempotent(t *testing.T) {
	s := New(quietSupervisorLog())
	s.Register(Spec{Name: "p", Command: []string{haveSleep(t), "60"}, StopTimeout: 2 * time.Second})

	a := s.Enter("p")
	b := s.Enter("p")
	if got := s.Inflight("p"); got != 2 {
		t.Fatalf("inflight=%d, want 2", got)
	}
	a()
	a() // idempotent
	b()
	b() // idempotent
	if got := s.Inflight("p"); got != 0 {
		t.Fatalf("inflight=%d after releases, want 0", got)
	}
}

// TestReapDoesNotKillBusyProcess targets the gap between "is it idle" and
// "stop it" in reapIdle.
//
// The judgement is narrow on purpose: a violation is a process observed in
// state running with inflight > 0 that then becomes stopped. Neither "PID
// changed" nor "inflight > 0 inside the onStop callback" works as a signal --
// a request that arrives while a legitimate stop is already in progress will
// show up in both, and that is a boundary the design accepts, not a bug.
func TestReapDoesNotKillBusyProcess(t *testing.T) {
	s := New(quietSupervisorLog())

	snapshotOf := func() (State, int, int) {
		for _, st := range s.Status() {
			if st.Name == "p" {
				return st.State, st.Inflight, st.PID
			}
		}
		return StateStopped, 0, 0
	}

	var killedWhileBusy atomic.Int64

	s.Register(Spec{
		Name:           "p",
		Command:        []string{haveSleep(t), "60"},
		IdleTimeout:    time.Nanosecond, // always eligible for reaping
		StopTimeout:    2 * time.Second,
		StartupTimeout: 5 * time.Second,
	})

	ctx := context.Background()
	if err := s.Ensure(ctx, "p"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	defer s.Shutdown(context.Background())

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// One goroutine keeps trying to reap.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				s.reapIdle()
			}
		}
	}()

	// The other keeps arriving with work, watching whether its process is
	// stopped while it is demonstrably serving.
	for i := 0; i < 400; i++ {
		if err := s.Ensure(ctx, "p"); err != nil {
			t.Fatalf("ensure during loop: %v", err)
		}
		release := s.Enter("p")

		serving := false
		for k := 0; k < 8; k++ {
			state, inflight, _ := snapshotOf()
			switch {
			// running or stopping with live work both count: the request is
			// still outstanding either way, and killing it truncates an answer.
			case (state == StateRunning || state == StateStopping) && inflight > 0:
				serving = true
			case serving && state == StateStopped:
				killedWhileBusy.Add(1)
				k = 8
			}
			time.Sleep(20 * time.Microsecond)
		}
		release()
	}
	close(stop)
	wg.Wait()

	if n := killedWhileBusy.Load(); n > 0 {
		t.Fatalf("reaper stopped a process %d time(s) while it was running with in-flight work", n)
	}
}

// TestConcurrentStatusDuringLifecycle is a race-detector target for Status,
// which walks the map, re-reads each entry and calls into proc.status while
// processes are being started and stopped underneath it.
func TestConcurrentStatusDuringLifecycle(t *testing.T) {
	s := New(quietSupervisorLog())
	for _, n := range []string{"a", "b", "c"} {
		s.Register(Spec{Name: n, Command: []string{haveSleep(t), "60"}, StopTimeout: 2 * time.Second})
	}

	ctx := context.Background()
	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Readers.
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = s.Status()
				_ = s.Names()
				_ = s.Inflight("a")
				_ = s.Has("b")
			}
		}()
	}

	// Writers.
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := []string{"a", "b", "c"}[i]
			for j := 0; j < 10; j++ {
				_ = s.Ensure(ctx, name)
				s.Touch(name)
				_ = s.Stop(name)
			}
		}(i)
	}

	time.Sleep(300 * time.Millisecond)
	close(stop)
	wg.Wait()
	s.Shutdown(context.Background())

	for _, st := range s.Status() {
		if st.Inflight != 0 {
			t.Fatalf("%s: inflight=%d at rest, want 0", st.Name, st.Inflight)
		}
	}
}
