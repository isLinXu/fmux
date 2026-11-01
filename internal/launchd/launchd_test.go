package launchd

import (
	"strings"
	"testing"
)

// printRunning is trimmed from real `launchctl print gui/501/<label>` output.
const printRunning = `gui/501/com.fmux.gateway = {
	active count = 1
	path = /Users/x/Library/LaunchAgents/com.fmux.gateway.plist
	type = LaunchAgent
	state = running

	program = /Users/x/.local/bin/fmux
	arguments = {
		/Users/x/.local/bin/fmux
		serve
	}

	pid = 87010
	last exit code = 0
}
`

func TestParsePrintRunning(t *testing.T) {
	st := ParsePrint(printRunning)
	if !st.Loaded {
		t.Fatal("expected loaded")
	}
	if !st.Running() {
		t.Fatal("expected running")
	}
	if st.PID != 87010 {
		t.Fatalf("pid = %d, want 87010", st.PID)
	}
	if st.Status != "running" {
		t.Fatalf("state = %q", st.Status)
	}
}

func TestParsePrintExited(t *testing.T) {
	// A crashed job keeps its plist but loses its pid, and the exit code is
	// what tells an operator whether to look at the config or at the binary.
	out := strings.Replace(printRunning, "\tpid = 87010\n", "", 1)
	out = strings.Replace(out, "state = running", "state = exited", 1)
	out = strings.Replace(out, "last exit code = 0", "last exit code = 78", 1)
	st := ParsePrint(out)
	if st.Running() {
		t.Fatal("a job without a pid is not running")
	}
	if st.LastExit != 78 {
		t.Fatalf("last exit = %d, want 78", st.LastExit)
	}
	if st.Status != "exited" {
		t.Fatalf("state = %q, want exited", st.Status)
	}
}

func TestRenderEscapesAndOrdersEnv(t *testing.T) {
	body, err := Render(Options{
		Label:  "com.example.a&b",
		Bin:    "/tmp/bin/fmux",
		Args:   []string{"serve", "--config", "/tmp/a&b.yaml"},
		Log:    "/tmp/fmux.log",
		KeepOn: true,
		Env:    map[string]string{"ZED": "1", "ALPHA": "2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, "a&b") {
		t.Fatal("ampersands must be escaped")
	}
	if !strings.Contains(body, "a&amp;b") {
		t.Fatal("expected escaped label")
	}
	// Map iteration in text/template is sorted, so the plist is stable across
	// runs and diffable.
	if strings.Index(body, "<key>ALPHA</key>") > strings.Index(body, "<key>ZED</key>") {
		t.Fatal("env keys must be sorted")
	}
	if !strings.Contains(body, "<key>SuccessfulExit</key>") {
		t.Fatal("keepalive should survive a crash")
	}
}

func TestRenderNoKeepAlive(t *testing.T) {
	body, err := Render(Options{Bin: "/tmp/fmux", Label: "x", Log: "/tmp/l"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, "SuccessfulExit") {
		t.Fatal("KeepAlive should be false when KeepOn is unset")
	}
	if !strings.Contains(body, "<string>x</string>") {
		t.Fatal("label missing")
	}
}
