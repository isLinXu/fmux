package metrics

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"
)

func render(t *testing.T, r *Registry) string {
	t.Helper()
	var buf bytes.Buffer
	if err := r.Render(&buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	return buf.String()
}

// TestHistogramBucketsAreCumulative: Prometheus reads bucket counters as
// "less than or equal to", so they have to be accumulated. Emitting the raw
// per-bucket counts produces a graph that goes down as latency goes up.
func TestHistogramBucketsAreCumulative(t *testing.T) {
	r := New()
	r.Observe("ollama", "qwen", 200, 10, 20, 120*time.Millisecond)
	text := render(t, r)

	for _, want := range []string{
		`fmux_request_duration_milliseconds_bucket{backend="ollama",le="100"} 0`,
		`fmux_request_duration_milliseconds_bucket{backend="ollama",le="250"} 1`,
		`fmux_request_duration_milliseconds_bucket{backend="ollama",le="+Inf"} 1`,
		`fmux_request_duration_milliseconds_sum{backend="ollama"} 120`,
		`fmux_request_duration_milliseconds_count{backend="ollama"} 1`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q\n%s", want, text)
		}
	}
}

// TestHistogramCountsAboveLastBucket: a generation that runs past the largest
// bucket must still appear in +Inf and in the count, or the histogram silently
// loses exactly the requests that were slow enough to matter.
func TestHistogramCountsAboveLastBucket(t *testing.T) {
	r := New()
	r.Observe("ollama", "qwen", 200, 0, 0, 10*time.Minute)
	text := render(t, r)

	last := BucketsMS[len(BucketsMS)-1]
	want := `fmux_request_duration_milliseconds_bucket{backend="ollama",le="` +
		formatFloat(last) + `"} 0`
	if !strings.Contains(text, want) {
		t.Errorf("missing %q\n%s", want, text)
	}
	if !strings.Contains(text, `fmux_request_duration_milliseconds_bucket{backend="ollama",le="+Inf"} 1`) {
		t.Errorf("oversized request missing from +Inf\n%s", text)
	}
}

func TestTTFTIsSeparateFromDuration(t *testing.T) {
	r := New()
	r.Observe("ollama", "qwen", 200, 0, 0, 5*time.Second)
	r.ObserveTTFT("ollama", 40*time.Millisecond)
	text := render(t, r)

	if !strings.Contains(text, `fmux_time_to_first_token_milliseconds_bucket{backend="ollama",le="50"} 1`) {
		t.Errorf("ttft not recorded in its own family\n%s", text)
	}
	// The duration histogram must not pick up the TTFT sample.
	if !strings.Contains(text, `fmux_request_duration_milliseconds_count{backend="ollama"} 1`) {
		t.Errorf("duration count wrong\n%s", text)
	}
}

func TestNilRegistryIsSafe(t *testing.T) {
	var r *Registry
	r.Observe("x", "y", 200, 0, 0, time.Second)
	r.ObserveTTFT("x", time.Second)
	r.Evicted()
	r.SetInflight("x", 1)
	r.QueueWait("x", time.Second)
	r.Rejected("x", "busy")
	var buf bytes.Buffer
	if err := r.Render(&buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("nil registry rendered %q", buf.String())
	}
}

func formatFloat(f float64) string {
	return fmt.Sprintf("%g", f)
}
