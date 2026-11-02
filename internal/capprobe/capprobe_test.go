package capprobe

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/isLinXu/fmux/internal/adapter"
	"github.com/isLinXu/fmux/internal/adapter/openai"
)

const okCompletion = `{"id":"c","object":"chat.completion","created":0,"model":"m",
"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`

// pickyBackend rejects exactly the request shapes named in reject, and answers
// everything else normally. That is what an older or differently-compiled
// server behaves like.
func pickyBackend(t *testing.T, reject ...string) adapter.Adapter {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		for _, feature := range reject {
			if _, present := body[feature]; present {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":{"message":"` + feature + ` is not supported by this build"}}`))
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okCompletion))
	}))
	t.Cleanup(srv.Close)

	return openai.New(openai.Options{
		Name:    "picky",
		Type:    "llamacpp",
		BaseURL: srv.URL + "/v1",
		Caps: adapter.Capabilities{
			Stream: true, JSONSchema: true, Tools: true, Vision: true, MaxContext: 32768,
		},
	})
}

func TestRunConfirmsADeclaredBackend(t *testing.T) {
	ad := pickyBackend(t) // rejects nothing
	rep := Run(context.Background(), ad, "m", 5*time.Second)

	if !rep.Baseline {
		t.Fatalf("baseline failed against a healthy backend: %+v", rep.Checks)
	}
	if !rep.Agrees() {
		t.Fatalf("expected full agreement, mismatches: %+v", rep.Mismatches())
	}
	if len(rep.Checks) != 3 {
		t.Fatalf("expected 3 capability checks, got %d", len(rep.Checks))
	}
}

// TestRunFindsARejectedCapability is the case the package exists for: the type
// says json_schema, the build says no, and the client would have found out by
// getting a 400 on a real request.
func TestRunFindsARejectedCapability(t *testing.T) {
	ad := pickyBackend(t, "response_format")
	rep := Run(context.Background(), ad, "m", 5*time.Second)

	if !rep.Baseline {
		t.Fatalf("baseline should still succeed: %+v", rep.Checks)
	}
	if rep.Agrees() {
		t.Fatal("expected a mismatch for json_schema")
	}

	var found bool
	for _, c := range rep.Mismatches() {
		if c.Capability == "json_schema" {
			found = true
			if !strings.Contains(c.Detail, "not supported") {
				t.Fatalf("detail should carry the backend's reason, got %q", c.Detail)
			}
		}
	}
	if !found {
		t.Fatalf("json_schema was not reported as mismatched: %+v", rep.Mismatches())
	}
}

func TestRunFindsRejectedTools(t *testing.T) {
	ad := pickyBackend(t, "tools")
	rep := Run(context.Background(), ad, "m", 5*time.Second)

	var found bool
	for _, c := range rep.Mismatches() {
		if c.Capability == "tools" {
			found = true
		}
	}
	if !found {
		t.Fatalf("tools was not reported as mismatched: %+v", rep.Checks)
	}
}

// TestRunStopsWhenTheBaselineFails keeps the report honest: if a plain request
// does not work, every later "rejected" verdict would be fiction.
func TestRunStopsWhenTheBaselineFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"message":"model not found"}}`))
	}))
	defer srv.Close()

	ad := openai.New(openai.Options{
		Name: "gone", Type: "llamacpp", BaseURL: srv.URL + "/v1",
		Caps: adapter.Capabilities{Stream: true, JSONSchema: true},
	})

	rep := Run(context.Background(), ad, "m", 3*time.Second)

	if rep.Baseline {
		t.Fatal("baseline should have failed")
	}
	if rep.Agrees() {
		t.Fatal("a failed baseline must not be reported as agreement")
	}
	if len(rep.Checks) != 1 || rep.Checks[0].Capability != "baseline" {
		t.Fatalf("expected only the baseline check, got %+v", rep.Checks)
	}
}

// TestRunRespectsTheDeadline makes sure a hung backend does not hang the probe.
func TestRunRespectsTheDeadline(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	ad := openai.New(openai.Options{
		Name: "slow", Type: "llamacpp", BaseURL: srv.URL + "/v1",
		Caps: adapter.Capabilities{Stream: true},
	})

	start := time.Now()
	rep := Run(context.Background(), ad, "m", 200*time.Millisecond)
	elapsed := time.Since(start)

	if elapsed > 3*time.Second {
		t.Fatalf("probe took %s against a hung backend", elapsed)
	}
	if rep.Baseline {
		t.Fatal("a hung backend must not be reported as a working baseline")
	}
}
