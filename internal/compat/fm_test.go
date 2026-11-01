package compat

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/isLinXu/fmux/internal/ir"
)

func loadGolden(t *testing.T, name string) *ir.ChatRequest {
	t.Helper()
	path := filepath.Join("..", "..", "test", "conformance", "golden", name+".json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}
	var req ir.ChatRequest
	if err := json.Unmarshal(b, &req); err != nil {
		t.Fatalf("parse golden %s: %v", name, err)
	}
	return &req
}

// TestGoldenNormalization replays every captured fm request through the
// compatibility layer and asserts the result is safe to forward.
func TestGoldenNormalization(t *testing.T) {
	cases := []struct {
		name       string
		wantQuirks []string
		wantMsgs   int
		keepSystem bool
	}{
		{name: "plain", wantQuirks: []string{QuirkNormalizedTools}, wantMsgs: 1},
		{name: "stream", wantQuirks: []string{QuirkNormalizedTools}, wantMsgs: 1},
		{name: "header", wantQuirks: []string{QuirkNormalizedTools}, wantMsgs: 1},
		{name: "instructions", wantQuirks: []string{QuirkNormalizedTools}, wantMsgs: 2, keepSystem: true},
		{name: "schema", wantQuirks: []string{QuirkNormalizedTools}, wantMsgs: 1},
		{name: "image", wantQuirks: []string{QuirkNormalizedTools}, wantMsgs: 1},
		{
			name:       "tool",
			wantQuirks: []string{QuirkNoContentField, QuirkDroppedEmptySystem, QuirkNormalizedTools},
			wantMsgs:   1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := loadGolden(t, tc.name)

			// Preconditions: fm always sends stream=true and an empty tools array.
			if !req.Stream {
				t.Error("golden should have stream=true")
			}
			if req.Tools == nil {
				t.Error("golden should carry an empty tools array")
			}
			if !req.WantsUsageInStream() {
				t.Error("golden should request usage in stream")
			}

			quirks, err := Normalize(req)
			if err != nil {
				t.Fatalf("normalize: %v", err)
			}

			for _, want := range tc.wantQuirks {
				if !contains(quirks, want) {
					t.Errorf("missing quirk %q in %v", want, quirks)
				}
			}
			if req.Tools != nil {
				t.Error("empty tools array should be dropped")
			}
			if len(req.Messages) != tc.wantMsgs {
				t.Fatalf("got %d messages, want %d", len(req.Messages), tc.wantMsgs)
			}
			for i, m := range req.Messages {
				if !m.Content.Present {
					t.Errorf("messages[%d] still has no content field", i)
				}
			}
			if tc.keepSystem && req.Messages[0].Role != ir.RoleSystem {
				t.Errorf("expected system message preserved, got %q", req.Messages[0].Role)
			}
			if tc.keepSystem && req.Messages[0].Content.PlainText() != "be terse" {
				t.Errorf("system content = %q", req.Messages[0].Content.PlainText())
			}
		})
	}
}

func TestLooksLikeFm(t *testing.T) {
	req := loadGolden(t, "plain")

	h := http.Header{}
	if LooksLikeFm(h, req) != true {
		t.Error("structural fingerprint should identify fm even without a User-Agent")
	}

	h.Set("User-Agent", "fm/1.0 (macOS)")
	if !LooksLikeFm(h, req) {
		t.Error("fm User-Agent should be detected")
	}

	h.Set("User-Agent", "curl/8.4.0")
	other := &ir.ChatRequest{Model: "m", Messages: req.Messages, Stream: false}
	if LooksLikeFm(h, other) {
		t.Error("a non-streaming request without fm markers should not match")
	}
}

func TestNormalizeRejectsEmpty(t *testing.T) {
	req := &ir.ChatRequest{Model: "m", Messages: []ir.Message{{Role: ir.RoleSystem}}}
	if _, err := Normalize(req); err == nil {
		t.Error("a request with only an empty system message should fail")
	}
}

func TestApplySystemPrefix(t *testing.T) {
	req := &ir.ChatRequest{Messages: []ir.Message{{Role: ir.RoleUser, Content: ir.Content{Text: "hi", Present: true}}}}
	ApplySystemPrefix(req, "be brief")
	if len(req.Messages) != 2 || req.Messages[0].Role != ir.RoleSystem {
		t.Fatalf("prefix not applied: %+v", req.Messages)
	}
	if req.Messages[0].Content.PlainText() != "be brief" {
		t.Errorf("prefix text = %q", req.Messages[0].Content.PlainText())
	}

	ApplySystemPrefix(req, "again")
	if req.Messages[0].Content.PlainText() != "again\n\nbe brief" {
		t.Errorf("existing system message should be extended, got %q", req.Messages[0].Content.PlainText())
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
