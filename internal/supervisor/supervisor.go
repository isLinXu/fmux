// Package supervisor owns the lifetime of local inference server processes.
//
// On macOS a local model is not just an HTTP endpoint: llama.cpp and MLX serve
// exactly one model per process, loading an 18 GB checkpoint takes tens of
// seconds, and leaving a model resident after the last request is how a 128 GB
// machine runs out of memory. None of that exists in a pure HTTP router, so the
// gateway has to manage child processes itself.
//
// The supervisor is deliberately dumb about models: it starts a command, waits
// for it to answer a health probe, and stops it when asked. Model-level policy
// (budget, LRU) lives in the budget package.
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// State is a child process lifecycle state.
type State string

const (
	StateStopped  State = "stopped"
	StateStarting State = "starting"
	StateRunning  State = "running"
	StateFailed   State = "failed"
)

// Defaults applied when a Spec leaves a duration at zero.
const (
	DefaultStartupTimeout = 90 * time.Second
	DefaultStopTimeout    = 10 * time.Second
	DefaultHealthInterval = 250 * time.Millisecond
)

// Spec describes one managed process.
type Spec struct {
	Name           string
	Command        []string
	Env            map[string]string
	Dir            string
	HealthURL      string // polled until it answers 2xx; empty means "assume ready"
	StartupTimeout time.Duration
	StopTimeout    time.Duration
	IdleTimeout    time.Duration // 0 disables idle reaping
	LogPath        string        // empty means inherit the gateway's stderr
}

// Status is a point-in-time view of one process.
type Status struct {
	Name      string    `json:"name"`
	State     State     `json:"state"`
	PID       int       `json:"pid,omitempty"`
	Command   []string  `json:"command,omitempty"`
	StartedAt time.Time `json:"started_at,omitempty"`
	LastUsed  time.Time `json:"last_used,omitempty"`
	Restarts  int       `json:"restarts"`
	LastError string    `json:"last_error,omitempty"`
}

// Supervisor manages a fixed set of named processes.
type Supervisor struct {
	mu     sync.RWMutex
	procs  map[string]*proc
	log    *slog.Logger
	onStop func(name string)

	stopOnce sync.Once
	stopped  chan struct{}
	wg       sync.WaitGroup
}

// SetOnStop registers a callback fired after any process stops, so the caller
// can drop its residency accounting for that backend.
func (s *Supervisor) SetOnStop(fn func(name string)) {
	s.mu.Lock()
	s.onStop = fn
	s.mu.Unlock()
}

func (s *Supervisor) notifyStop(name string) {
	s.mu.RLock()
	fn := s.onStop
	s.mu.RUnlock()
	if fn != nil {
		fn(name)
	}
}

// New returns a supervisor with no registered processes.
func New(log *slog.Logger) *Supervisor {
	if log == nil {
		log = slog.Default()
	}
	return &Supervisor{
		procs:   map[string]*proc{},
		log:     log,
		stopped: make(chan struct{}),
	}
}

// Register adds a process spec. It must be called before StartWatcher.
func (s *Supervisor) Register(spec Spec) {
	spec.StartupTimeout = orDefault(spec.StartupTimeout, DefaultStartupTimeout)
	spec.StopTimeout = orDefault(spec.StopTimeout, DefaultStopTimeout)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.procs[spec.Name] = &proc{spec: spec, state: StateStopped}
}

// Has reports whether a name is managed by this supervisor.
func (s *Supervisor) Has(name string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.procs[name]
	return ok
}

// Names lists managed process names.
func (s *Supervisor) Names() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.procs))
	for n := range s.procs {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Ensure starts the named process if needed and waits until it is serving.
func (s *Supervisor) Ensure(ctx context.Context, name string) error {
	s.mu.RLock()
	p := s.procs[name]
	s.mu.RUnlock()
	if p == nil {
		return fmt.Errorf("supervisor: unknown process %q", name)
	}
	return p.ensure(ctx, s.log)
}

// Stop terminates the named process, if running.
func (s *Supervisor) Stop(name string) error {
	s.mu.RLock()
	p := s.procs[name]
	s.mu.RUnlock()
	if p == nil {
		return fmt.Errorf("supervisor: unknown process %q", name)
	}
	p.mu.Lock()
	err := p.stopLocked(s.log)
	p.mu.Unlock()
	if err == nil {
		s.notifyStop(name)
	}
	return err
}

// Touch records activity so idle reaping leaves the process alone.
func (s *Supervisor) Touch(name string) {
	s.mu.RLock()
	p := s.procs[name]
	s.mu.RUnlock()
	if p == nil {
		return
	}
	p.mu.Lock()
	p.lastUsed = time.Now()
	p.mu.Unlock()
}

// Status returns every process state, sorted by name.
func (s *Supervisor) Status() []Status {
	s.mu.RLock()
	names := make([]string, 0, len(s.procs))
	for n := range s.procs {
		names = append(names, n)
	}
	s.mu.RUnlock()
	sort.Strings(names)

	out := make([]Status, 0, len(names))
	for _, n := range names {
		s.mu.RLock()
		p := s.procs[n]
		s.mu.RUnlock()
		if p == nil {
			continue
		}
		out = append(out, p.status())
	}
	return out
}

// StartWatcher reaps idle processes until ctx is cancelled.
func (s *Supervisor) StartWatcher(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-s.stopped:
				return
			case <-t.C:
				s.reapIdle()
			}
		}
	}()
}

func (s *Supervisor) reapIdle() {
	s.mu.RLock()
	procs := make([]*proc, 0, len(s.procs))
	for _, p := range s.procs {
		procs = append(procs, p)
	}
	s.mu.RUnlock()

	now := time.Now()
	for _, p := range procs {
		p.mu.Lock()
		idle := p.spec.IdleTimeout
		running := p.state == StateRunning
		last := p.lastUsed
		name := p.spec.Name
		p.mu.Unlock()

		if !running || idle <= 0 || last.IsZero() || now.Sub(last) < idle {
			continue
		}
		s.log.Info("reaping idle backend", "backend", name, "idle", now.Sub(last).Round(time.Second))
		if err := s.Stop(name); err != nil {
			s.log.Warn("reap failed", "backend", name, "err", err)
		}
	}
}

// Shutdown stops every managed process and the watcher.
func (s *Supervisor) Shutdown(ctx context.Context) {
	s.stopOnce.Do(func() { close(s.stopped) })
	s.wg.Wait()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for _, n := range s.Names() {
			if err := s.Stop(n); err != nil {
				s.log.Warn("stop failed", "backend", n, "err", err)
			}
		}
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// proc is one managed child process.
type proc struct {
	spec Spec

	mu        sync.Mutex
	cmd       *exec.Cmd
	state     State
	pid       int
	startedAt time.Time
	lastUsed  time.Time
	restarts  int
	lastErr   string

	exited atomic.Bool
	done   chan struct{}
}

func (p *proc) status() Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	return Status{
		Name:      p.spec.Name,
		State:     p.state,
		PID:       p.pid,
		Command:   append([]string(nil), p.spec.Command...),
		StartedAt: p.startedAt,
		LastUsed:  p.lastUsed,
		Restarts:  p.restarts,
		LastError: p.lastErr,
	}
}

func (p *proc) ensure(ctx context.Context, log *slog.Logger) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.spec.Command) == 0 {
		return nil // externally managed backend
	}
	if p.state == StateRunning && !p.exited.Load() {
		p.lastUsed = time.Now()
		return nil
	}
	if p.cmd != nil {
		p.stopLocked(log)
	}
	return p.startLocked(ctx, log)
}

func (p *proc) startLocked(ctx context.Context, log *slog.Logger) error {
	p.state = StateStarting
	p.startedAt = time.Now()
	p.lastUsed = p.startedAt
	p.lastErr = ""
	p.exited.Store(false)
	p.done = make(chan struct{})

	cmd := exec.Command(p.spec.Command[0], p.spec.Command[1:]...)
	cmd.Env = mergeEnv(p.spec.Env)
	cmd.Dir = p.spec.Dir
	out, closer, err := p.openLog()
	if err != nil {
		p.state = StateFailed
		p.lastErr = err.Error()
		return err
	}
	if closer != nil {
		defer closer.Close()
	}
	cmd.Stdout = out
	cmd.Stderr = out

	if err := cmd.Start(); err != nil {
		p.state = StateFailed
		p.lastErr = err.Error()
		return fmt.Errorf("start %s: %w", p.spec.Name, err)
	}
	p.cmd = cmd
	p.pid = cmd.Process.Pid
	if p.restarts > 0 || !p.startedAt.IsZero() {
		// restarts counts subsequent launches, not the first one
	}
	go p.wait(cmd, p.done, log)

	log.Info("backend process started", "backend", p.spec.Name, "pid", p.pid, "command", strings.Join(p.spec.Command, " "))

	if p.spec.HealthURL == "" {
		p.state = StateRunning
		return nil
	}
	if err := p.awaitHealth(ctx); err != nil {
		p.stopLocked(log)
		p.state = StateFailed
		p.lastErr = err.Error()
		return err
	}
	p.state = StateRunning
	log.Info("backend ready", "backend", p.spec.Name, "pid", p.pid)
	return nil
}

func (p *proc) awaitHealth(ctx context.Context) error {
	deadline := time.Now().Add(p.spec.StartupTimeout)
	client := &http.Client{Timeout: 2 * time.Second}
	var last error
	for {
		if p.exited.Load() {
			return fmt.Errorf("%s exited during startup", p.spec.Name)
		}
		if err := probe(ctx, client, p.spec.HealthURL); err == nil {
			return nil
		} else {
			last = err
		}
		if time.Now().After(deadline) {
			if last == nil {
				last = errors.New("timeout")
			}
			return fmt.Errorf("%s did not become healthy within %s: %w", p.spec.Name, p.spec.StartupTimeout, last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-p.done:
			return fmt.Errorf("%s exited during startup", p.spec.Name)
		case <-time.After(DefaultHealthInterval):
		}
	}
}

func (p *proc) stopLocked(log *slog.Logger) error {
	if p.cmd == nil || p.cmd.Process == nil || p.exited.Load() {
		p.state = StateStopped
		p.pid = 0
		return nil
	}
	name := p.spec.Name
	pid := p.pid
	_ = p.cmd.Process.Signal(syscall.SIGTERM)

	select {
	case <-p.done:
	case <-time.After(p.spec.StopTimeout):
		log.Warn("backend did not exit, killing", "backend", name, "pid", pid)
		_ = p.cmd.Process.Kill()
		<-p.done
	}
	p.state = StateStopped
	p.pid = 0
	p.cmd = nil
	log.Info("backend process stopped", "backend", name, "pid", pid)
	return nil
}

func (p *proc) wait(cmd *exec.Cmd, done chan struct{}, log *slog.Logger) {
	err := cmd.Wait()
	p.exited.Store(true)
	close(done)

	p.mu.Lock()
	wasStopping := p.state == StateStopped
	if !wasStopping {
		p.restarts++
		if err != nil {
			p.state = StateFailed
			p.lastErr = err.Error()
		} else {
			p.state = StateStopped
			p.lastErr = "exited with status 0"
		}
	}
	p.pid = 0
	p.mu.Unlock()

	if wasStopping {
		return
	}
	if err != nil {
		log.Warn("backend process exited", "backend", p.spec.Name, "err", err)
	} else {
		log.Warn("backend process exited", "backend", p.spec.Name)
	}
}

func (p *proc) openLog() (io.Writer, io.Closer, error) {
	if p.spec.LogPath == "" {
		return os.Stderr, nil, nil
	}
	if err := os.MkdirAll(filepath.Dir(p.spec.LogPath), 0o755); err != nil {
		return nil, nil, err
	}
	f, err := os.OpenFile(p.spec.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, nil, err
	}
	return f, f, nil
}

func probe(ctx context.Context, client *http.Client, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("health returned %s", resp.Status)
	}
	return nil
}

func mergeEnv(extra map[string]string) []string {
	env := os.Environ()
	if len(extra) == 0 {
		return env
	}
	keys := make([]string, 0, len(extra))
	for k := range extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+extra[k])
	}
	return env
}

func orDefault(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}
