// Package launchd manages a per-user LaunchAgent for the gateway.
//
// launchd is the only supported way to keep fmux running across logins on
// macOS; a background shell dies with its parent. This package deliberately
// stays thin: it renders a plist, hands it to launchctl, and reads back the
// job's state.
package launchd

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"
	"time"
)

// DefaultLabel is the job identifier used unless the user overrides it.
const DefaultLabel = "com.fmux.gateway"

// ErrNotLoaded means the job is not present in the user's launchd domain.
var ErrNotLoaded = errors.New("launchd: job is not loaded")

// Options describes the agent to write.
type Options struct {
	Label  string
	Bin    string   // absolute path to the fmux binary
	Args   []string // arguments after the binary path
	Log    string   // combined stdout/stderr path
	Env    map[string]string
	KeepOn bool // restart after a crash

	// ExitTimeout is how long launchd waits after SIGTERM before SIGKILL.
	//
	// It has to cover the gateway's own drain window, or the drain is
	// theatre: launchd's default is 20 seconds, which would SIGKILL a process
	// that is deliberately still serving a long generation.
	ExitTimeout int // seconds; 0 leaves launchd's default
}

// PlistPath returns ~/Library/LaunchAgents/<label>.plist.
func PlistPath(label string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", label+".plist"), nil
}

// DefaultLogPath returns ~/Library/Logs/fmux.log.
func DefaultLogPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "Library", "Logs", "fmux.log")
}

var plistTmpl = template.Must(template.New("plist").Funcs(template.FuncMap{
	"xml": func(s string) string {
		r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
		return r.Replace(s)
	},
}).Parse(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>{{xml .Label}}</string>
	<key>ProgramArguments</key>
	<array>
		<string>{{xml .Bin}}</string>
{{- range .Args}}
		<string>{{xml .}}</string>
{{- end}}
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
{{- if .KeepOn}}
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
{{- else}}
	<false/>
{{- end}}
	<key>ProcessType</key>
	<string>Interactive</string>
{{- if .ExitTimeout}}
	<key>ExitTimeOut</key>
	<integer>{{.ExitTimeout}}</integer>
{{- end}}
	<key>StandardOutPath</key>
	<string>{{xml .Log}}</string>
	<key>StandardErrorPath</key>
	<string>{{xml .Log}}</string>
	<key>EnvironmentVariables</key>
	<dict>
		<key>PATH</key>
		<string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
{{- range $k, $v := .Env}}
		<key>{{xml $k}}</key>
		<string>{{xml $v}}</string>
{{- end}}
	</dict>
</dict>
</plist>
`))

// Render produces the plist body.
func Render(o Options) (string, error) {
	if o.Label == "" {
		o.Label = DefaultLabel
	}
	if o.Bin == "" {
		return "", errors.New("launchd: bin path is required")
	}
	if o.Log == "" {
		o.Log = DefaultLogPath()
	}
	var buf bytes.Buffer
	if err := plistTmpl.Execute(&buf, o); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// Write renders the plist and puts it in ~/Library/LaunchAgents, returning the
// path. It does not load the job.
func Write(o Options) (string, error) {
	body, err := Render(o)
	if err != nil {
		return "", err
	}
	path, err := PlistPath(o.Label)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if o.Log != "" {
		if err := os.MkdirAll(filepath.Dir(o.Log), 0o755); err != nil {
			return "", err
		}
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// Domain is the per-user launchd domain, e.g. gui/501.
func Domain() string { return "gui/" + strconv.Itoa(os.Getuid()) }

// Load asks launchd to take ownership of the plist.
//
// `launchctl bootstrap` reports "Input/output error" (EIO, exit 5) for a job
// that is already loaded, so EIO is only an error when the job turns out not
// to be running. Callers get ErrNotLoaded when the job is genuinely absent,
// which is what a caller needs to distinguish "already fine" from "blocked".
func Load(label, plistPath string) error {
	out, err := run("bootstrap", Domain(), plistPath)
	if err == nil {
		return nil
	}
	if st, serr := Status(label); serr == nil && st.Loaded {
		return nil
	}
	return fmt.Errorf("launchctl bootstrap: %s", strings.TrimSpace(out))
}

// Unload removes the job from the domain. A job that is not loaded is not an
// error.
func Unload(label string) error {
	out, err := run("bootout", Domain()+"/"+label)
	if err == nil {
		return nil
	}
	if _, serr := Status(label); errors.Is(serr, ErrNotLoaded) {
		return nil
	}
	return fmt.Errorf("launchctl bootout: %s", strings.TrimSpace(out))
}

// Kickstart restarts a loaded job, killing the current process.
func Kickstart(label string) error {
	out, err := run("kickstart", "-k", Domain()+"/"+label)
	if err != nil {
		return fmt.Errorf("launchctl kickstart: %s", strings.TrimSpace(out))
	}
	return nil
}

// Signal delivers a signal to the job's process.
func Signal(label, sig string) error {
	out, err := run("kill", sig, Domain()+"/"+label)
	if err != nil {
		return fmt.Errorf("launchctl kill %s: %s", sig, strings.TrimSpace(out))
	}
	return nil
}

// Restart stops the job with SIGTERM and starts it again.
//
// `kickstart -k` would be one line, but it kills the process outright: the
// gateway never gets to drain, so every generation in flight is truncated.
// Asking politely and waiting for the job to actually go away is what makes a
// restart look like a restart rather than a crash.
func Restart(label string, wait time.Duration) error {
	st, err := Status(label)
	if err != nil {
		return err
	}
	if st.Running() {
		if err := Signal(label, "SIGTERM"); err != nil {
			return err
		}
		deadline := time.Now().Add(wait)
		for time.Now().Before(deadline) {
			cur, err := Status(label)
			if err != nil || !cur.Running() {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if cur, err := Status(label); err == nil && cur.Running() {
			return fmt.Errorf("launchd: job %s still running after %s; in-flight requests are draining, try again or raise timeouts.drain", label, wait)
		}
	}
	return Kickstart(label)
}

// State is what `launchctl print` says about a job.
type State struct {
	Loaded   bool
	PID      int    // 0 when the job is loaded but not running
	Status   string // raw `state =` value: running, waiting, exited...
	LastExit int
}

// Running reports whether the job currently has a process.
func (s State) Running() bool { return s.Loaded && s.PID > 0 }

// Status reads the job state from launchd.
func Status(label string) (State, error) {
	out, err := run("print", Domain()+"/"+label)
	if err != nil {
		if strings.Contains(out, "Could not find service") || strings.Contains(out, "Bad request") {
			return State{}, ErrNotLoaded
		}
		return State{}, fmt.Errorf("launchctl print: %s", strings.TrimSpace(out))
	}
	return ParsePrint(out), nil
}

// ParsePrint extracts the fields we care about from `launchctl print` output.
// The format is not documented but is stable enough to scan line by line.
func ParsePrint(out string) State {
	st := State{Loaded: true}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		switch key {
		case "state":
			st.Status = val
		case "pid":
			if n, err := strconv.Atoi(val); err == nil {
				st.PID = n
			}
		case "last exit code":
			if n, err := strconv.Atoi(val); err == nil {
				st.LastExit = n
			}
		}
	}
	return st
}

func run(args ...string) (string, error) {
	cmd := exec.Command("launchctl", args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}
