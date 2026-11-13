package metrics

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestExportAggregatesTokens(t *testing.T) {
	r := New()
	r.Observe("b", "m", 200, 10, 20, time.Millisecond)
	r.Observe("b", "m", 502, 3, 4, time.Millisecond)
	r.Observe("a", "other", 200, 7, 8, time.Millisecond)
	r.Observe("b", "other", 429, 0, 0, time.Millisecond)
	text := render(t, r)
	assertUniqueSamples(t, text)
	for _, want := range []string{
		`fmux_requests_total{backend="b",model="m",status="200"} 1`,
		`fmux_requests_total{backend="b",model="m",status="502"} 1`,
		`fmux_prompt_tokens_total{backend="b",model="m"} 13`,
		`fmux_completion_tokens_total{backend="b",model="m"} 24`,
		`fmux_prompt_tokens_total{backend="a",model="other"} 7`,
		`fmux_completion_tokens_total{backend="a",model="other"} 8`,
		`fmux_prompt_tokens_total{backend="b",model="other"} 0`,
		`fmux_completion_tokens_total{backend="b",model="other"} 0`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %s", want)
		}
	}
}

func TestExportWriterErrors(t *testing.T) {
	sentinel := errors.New("write failed")
	for _, populated := range []bool{false, true} {
		r := New()
		if populated {
			for i := 0; i < 100; i++ {
				r.Observe(fmt.Sprint(i), "model", 200, 1, 2, time.Second)
			}
		}
		if err := r.Render(failingWriter{sentinel}); !errors.Is(err, sentinel) {
			t.Errorf("populated=%t err=%v want=%v", populated, err, sentinel)
		}
		if err := r.Render(shortWriter{}); !errors.Is(err, io.ErrShortWrite) {
			t.Errorf("populated=%t short write err=%v", populated, err)
		}
	}
	var nilRegistry *Registry
	if err := nilRegistry.Render(failingWriter{sentinel}); err != nil {
		t.Fatal(err)
	}
}

func TestExportStableAndEscapedLabels(t *testing.T) {
	r := New()
	for _, backend := range []string{"z", "a", "quote\"slash\\newline\n"} {
		for _, model := range []string{"z", "a"} {
			r.Observe(backend, model, 502, 1, 2, time.Second)
			r.Observe(backend, model, 200, 3, 4, time.Second)
		}
		r.SetInflight(backend, 1)
		r.QueueWait(backend, time.Millisecond)
		r.Rejected(backend, "full")
		r.ObserveTTFT(backend, time.Millisecond)
	}
	text := render(t, r)
	assertUniqueSamples(t, text)
	if !strings.Contains(text, `backend="quote\"slash\\newline\n"`) {
		t.Errorf("label escaping changed: %s", text)
	}
	// Uptime can cross a second between scrapes; the sample ordering cannot.
	withoutUptime := func(s string) string {
		return s[:strings.LastIndex(s, "fmux_uptime_seconds ")]
	}
	for i := 0; i < 10; i++ {
		if withoutUptime(render(t, r)) != withoutUptime(text) {
			t.Fatal("output ordering changed")
		}
	}
	for _, family := range []string{"fmux_requests_total", "fmux_prompt_tokens_total", "fmux_completion_tokens_total"} {
		var previous string
		for _, line := range strings.Split(text, "\n") {
			if strings.HasPrefix(line, family+"{") {
				if line < previous {
					t.Fatalf("unsorted family %s", family)
				}
				previous = line
			}
		}
	}
}

func TestExportConcurrentSnapshots(t *testing.T) {
	r := New()
	var wg sync.WaitGroup
	const workers, observations = 4, 300
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < observations; j++ {
				r.Observe("b", "m", 200+j%2, 1, 2, time.Millisecond)
				r.ObserveTTFT("b", time.Millisecond)
				r.QueueWait("b", time.Millisecond)
				r.Rejected("b", "full")
				r.SetInflight("b", j%2)
			}
		}()
	}
	close(start)
	for i := 0; i < 50; i++ {
		text := render(t, r)
		assertUniqueSamples(t, text)
		samples := sampleValues(t, text)
		count := samples[`fmux_request_duration_milliseconds_count{backend="b"}`]
		if samples[`fmux_request_duration_milliseconds_bucket{backend="b",le="+Inf"}`] != count ||
			samples[`fmux_request_duration_milliseconds_sum{backend="b"}`] != count {
			t.Error("histogram snapshot is internally inconsistent")
		}
	}
	wg.Wait()
	samples := sampleValues(t, render(t, r))
	for name, want := range map[string]int64{
		`fmux_prompt_tokens_total{backend="b",model="m"}`:          workers * observations,
		`fmux_completion_tokens_total{backend="b",model="m"}`:      2 * workers * observations,
		`fmux_requests_total{backend="b",model="m",status="200"}`:  workers * observations / 2,
		`fmux_requests_total{backend="b",model="m",status="201"}`:  workers * observations / 2,
		`fmux_request_duration_milliseconds_count{backend="b"}`:    workers * observations,
		`fmux_time_to_first_token_milliseconds_count{backend="b"}`: workers * observations,
		`fmux_queue_waits_total{backend="b"}`:                      workers * observations,
		`fmux_rejected_total{backend="b",reason="full"}`:           workers * observations,
	} {
		if samples[name] != want {
			t.Errorf("%s=%d want=%d", name, samples[name], want)
		}
	}
}

func TestHistogramCopyIsIndependent(t *testing.T) {
	h := newHist()
	h.add(time.Millisecond)
	copy := copyHists(map[string]*hist{"b": h})
	h.add(time.Millisecond)
	if copy["b"].counts[0] != 1 || copy["b"].count != 1 || copy["b"].sumMS != 1 {
		t.Fatal("histogram snapshot aliases live data")
	}
}

func assertUniqueSamples(t *testing.T, text string) {
	t.Helper()
	seen := map[string]bool{}
	for _, line := range strings.Split(text, "\n") {
		if line == "" || line[0] == '#' {
			continue
		}
		key := line[:strings.LastIndexByte(line, ' ')]
		if seen[key] {
			t.Errorf("duplicate sample: %s", key)
		}
		seen[key] = true
	}
}

func sampleValues(t *testing.T, text string) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	for _, line := range strings.Split(text, "\n") {
		if line == "" || line[0] == '#' {
			continue
		}
		i := strings.LastIndexByte(line, ' ')
		value, err := strconv.ParseInt(line[i+1:], 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		out[line[:i]] = value
	}
	return out
}

type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }

func BenchmarkExport(b *testing.B) {
	newRegistry := func() *Registry {
		r := New()
		for backend := 0; backend < 8; backend++ {
			for model := 0; model < 16; model++ {
				for _, status := range []int{200, 502} {
					r.Observe(fmt.Sprint(backend), fmt.Sprint(model), status, 10, 20, time.Second)
				}
			}
		}
		return r
	}
	b.Run("Render", func(b *testing.B) {
		r := newRegistry()
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if err := r.Render(io.Discard); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("RecordAndScrape", func(b *testing.B) {
		r := newRegistry()
		b.ReportAllocs()
		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			var n int
			for pb.Next() {
				r.Observe("0", "0", 200, 1, 2, time.Millisecond)
				n++
				if n%32 == 0 {
					var buf bytes.Buffer
					if err := r.Render(&buf); err != nil {
						b.Error(err)
					}
				}
			}
		})
	})
}
