// Package mock provides a deterministic in-process adapter. It exists so the
// gateway, the CLI and the conformance suite can be exercised without any real
// inference server running.
package mock

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/isLinXu/fmux/internal/adapter"
	"github.com/isLinXu/fmux/internal/ir"
)

// Adapter is a fake backend that echoes the prompt.
type Adapter struct {
	name  string
	caps  adapter.Capabilities
	delay time.Duration

	mu   sync.Mutex
	last *ir.ChatRequest
}

// New builds a mock adapter named "mock".
func New(name string) *Adapter {
	return &Adapter{
		name: name,
		caps: adapter.Capabilities{
			Stream: true, Vision: true, JSONSchema: true, Tools: true,
			UsageInStream: true, MaxContext: 8192,
		},
	}
}

// Name returns the backend name.
func (a *Adapter) Name() string { return a.name }

// Type returns "mock".
func (a *Adapter) Type() string { return "mock" }

// Caps returns the declared capabilities.
func (a *Adapter) Caps() adapter.Capabilities { return a.caps }

// LastRequest returns the most recent request the adapter saw.
func (a *Adapter) LastRequest() *ir.ChatRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.last
}

// Models lists the fake models.
func (a *Adapter) Models(ctx context.Context) ([]ir.ModelInfo, error) {
	return []ir.ModelInfo{
		{ID: "mock-model", Object: "model", OwnedBy: "fmux", Backend: a.name, ContextLen: 8192},
		{ID: "mock-vision", Object: "model", OwnedBy: "fmux", Backend: a.name, ContextLen: 8192, Tags: []string{"vision"}},
	}, nil
}

// Health always succeeds.
func (a *Adapter) Health(ctx context.Context) error { return nil }

// Chat returns a single echo completion.
func (a *Adapter) Chat(ctx context.Context, req *ir.ChatRequest) (*ir.ChatResponse, error) {
	a.record(req)
	out := ir.NewChatResponse(req.Model)
	text := a.reply(req)
	out.Choices = []ir.ResponseChoice{{
		Index:        0,
		Message:      ir.Message{Role: ir.RoleAssistant, Content: ir.Content{Text: text, Present: true}},
		FinishReason: strptr("stop"),
	}}
	out.Usage = usageFor(req, text)
	return out, nil
}

// ChatStream streams the echo reply word by word.
func (a *Adapter) ChatStream(ctx context.Context, req *ir.ChatRequest) (adapter.Stream, error) {
	a.record(req)
	text := a.reply(req)
	chunks := make([]*ir.Chunk, 0, 8)

	first := ir.NewChunk(req.Model)
	first.Choices = []ir.ChunkChoice{{Index: 0, Delta: ir.Delta{Role: "assistant", Content: ""}}}
	chunks = append(chunks, first)

	for _, w := range splitWords(text) {
		c := ir.NewChunk(req.Model)
		c.Choices = []ir.ChunkChoice{{Index: 0, Delta: ir.Delta{Content: w}}}
		chunks = append(chunks, c)
	}

	final := ir.NewChunk(req.Model)
	final.Choices = []ir.ChunkChoice{{Index: 0, Delta: ir.Delta{}, FinishReason: strptr("stop")}}
	if req.WantsUsageInStream() && a.caps.UsageInStream {
		final.Usage = usageFor(req, text)
	}
	chunks = append(chunks, final)

	return &sliceStream{chunks: chunks, delay: a.delay}, nil
}

func (a *Adapter) record(req *ir.ChatRequest) {
	a.mu.Lock()
	a.last = req.Clone()
	a.mu.Unlock()
}

// reply builds the deterministic answer.
func (a *Adapter) reply(req *ir.ChatRequest) string {
	if req.WantsJSONSchema() {
		if b, err := sampleJSON(req.ResponseFormat.JSONSchema.Schema); err == nil {
			return string(b)
		}
		return `{"error":"schema unsupported by mock"}`
	}
	prompt := lastUserText(req)
	if prompt == "" {
		prompt = "(empty)"
	}
	return "mock: " + prompt
}

func lastUserText(req *ir.ChatRequest) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == ir.RoleUser {
			return req.Messages[i].Content.PlainText()
		}
	}
	return ""
}

// sampleJSON builds a zero-value instance from a JSON schema.
func sampleJSON(schema json.RawMessage) ([]byte, error) {
	if len(schema) == 0 {
		return nil, fmt.Errorf("empty schema")
	}
	var s map[string]any
	if err := json.Unmarshal(schema, &s); err != nil {
		return nil, err
	}
	v := sampleValue(s)
	return json.Marshal(v)
}

func sampleValue(s map[string]any) any {
	typ, _ := s["type"].(string)
	switch typ {
	case "object":
		out := map[string]any{}
		if props, ok := s["properties"].(map[string]any); ok {
			for k, raw := range props {
				if sub, ok := raw.(map[string]any); ok {
					out[k] = sampleValue(sub)
				}
			}
		}
		return out
	case "array":
		if items, ok := s["items"].(map[string]any); ok {
			return []any{sampleValue(items)}
		}
		return []any{}
	case "integer":
		return 0
	case "number":
		return 0
	case "boolean":
		return false
	default:
		return ""
	}
}

func usageFor(req *ir.ChatRequest, text string) *ir.Usage {
	p := 0
	for _, m := range req.Messages {
		p += len(strings.Fields(m.Content.PlainText()))
	}
	c := len(strings.Fields(text))
	return &ir.Usage{PromptTokens: p, CompletionTokens: c, TotalTokens: p + c}
}

func splitWords(s string) []string {
	if s == "" {
		return nil
	}
	words := strings.SplitAfter(s, " ")
	out := make([]string, 0, len(words))
	for _, w := range words {
		if w != "" {
			out = append(out, w)
		}
	}
	return out
}

func strptr(s string) *string { return &s }

// sliceStream replays pre-built chunks.
type sliceStream struct {
	chunks []*ir.Chunk
	idx    int
	delay  time.Duration
}

func (s *sliceStream) Recv() (*ir.Chunk, error) {
	if s.idx >= len(s.chunks) {
		return nil, io.EOF
	}
	if s.delay > 0 {
		time.Sleep(s.delay)
	}
	c := s.chunks[s.idx]
	s.idx++
	return c, nil
}

func (s *sliceStream) Close() error { return nil }
