package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/isLinXu/fmux/internal/adapter"
	"github.com/isLinXu/fmux/internal/ir"
)

func TestChatWireOptions(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, originalStream := range []bool{false, true} {
			for _, usage := range []bool{false, true} {
				for _, supported := range []bool{false, true} {
					name := fmt.Sprintf("stream=%t/original=%t/usage=%t/supported=%t", stream, originalStream, usage, supported)
					t.Run(name, func(t *testing.T) {
						var received []byte
						ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							received, _ = io.ReadAll(r.Body)
							if stream {
								w.Header().Set("Content-Type", "text/event-stream")
								_, _ = io.WriteString(w, "data: [DONE]\n\n")
							} else {
								_, _ = io.WriteString(w, `{"choices":[]}`)
							}
						}))
						defer ts.Close()
						temp := 0.2
						req := &ir.ChatRequest{
							Model: "model", Stream: originalStream,
							StreamOptions: &ir.StreamOptions{IncludeUsage: usage},
							Temperature:   &temp,
							Messages: []ir.Message{{Role: ir.RoleUser, Content: ir.Content{
								IsParts: true, Present: true,
								Parts: []ir.ContentPart{
									{Type: "text", Text: "hello"},
									{Type: "image_url", ImageURL: &ir.ImageURL{URL: "data:image/jpeg;base64,AAAA"}},
								},
							}}},
							Tools: []ir.Tool{{Type: "function", Function: ir.ToolFuncDef{
								Name: "tool", Parameters: json.RawMessage(`{"type":"object"}`),
							}}},
							ToolChoice: json.RawMessage(`"auto"`),
							ResponseFormat: &ir.ResponseFormat{Type: "json_schema", JSONSchema: &ir.JSONSchemaSpec{
								Name: "result", Schema: json.RawMessage(`{"type":"object"}`),
							}},
							ResolvedAlias: "private", ResolvedBackend: "private",
							ResolvedModel: "private", ClientIsFm: true, Quirks: []string{"private"},
						}
						before, err := json.Marshal(req)
						if err != nil {
							t.Fatal(err)
						}
						a := New(Options{BaseURL: ts.URL, Client: ts.Client(), Caps: adapter.Capabilities{UsageInStream: supported}})
						if stream {
							st, err := a.ChatStream(context.Background(), req)
							if err != nil {
								t.Fatal(err)
							}
							_ = st.Close()
						} else if _, err := a.Chat(context.Background(), req); err != nil {
							t.Fatal(err)
						}
						after, err := json.Marshal(req)
						if err != nil || !bytes.Equal(before, after) {
							t.Fatalf("request mutated: before=%s after=%s err=%v", before, after, err)
						}
						assertUniqueKeys(t, received)
						var got, want map[string]json.RawMessage
						if err := json.Unmarshal(received, &got); err != nil {
							t.Fatal(err)
						}
						if err := json.Unmarshal(before, &want); err != nil {
							t.Fatal(err)
						}
						want["stream"] = json.RawMessage(fmt.Sprint(stream))
						if !stream || !usage || !supported {
							delete(want, "stream_options")
						}
						gotJSON, _ := json.Marshal(got)
						wantJSON, _ := json.Marshal(want)
						if !bytes.Equal(gotJSON, wantJSON) {
							t.Fatalf("wire mismatch:\ngot  %s\nwant %s", gotJSON, wantJSON)
						}
					})
				}
			}
		}
	}
}

func assertUniqueKeys(t *testing.T, body []byte) {
	t.Helper()
	d := json.NewDecoder(bytes.NewReader(body))
	if _, err := d.Token(); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for d.More() {
		token, err := d.Token()
		if err != nil {
			t.Fatal(err)
		}
		key := token.(string)
		if seen[key] {
			t.Fatalf("duplicate wire key %q", key)
		}
		seen[key] = true
		var value json.RawMessage
		if err := d.Decode(&value); err != nil {
			t.Fatal(err)
		}
	}
}

func TestInvalidRawJSONDoesNotReachBackend(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			transport := &countTransport{}
			a := New(Options{BaseURL: "http://backend", Client: &http.Client{Transport: transport}})
			req := &ir.ChatRequest{Model: "model", ToolChoice: json.RawMessage(`{`)}
			var err error
			if stream {
				_, err = a.ChatStream(context.Background(), req)
			} else {
				_, err = a.Chat(context.Background(), req)
			}
			if err == nil || transport.calls != 0 {
				t.Fatalf("err=%v backend calls=%d", err, transport.calls)
			}
		})
	}
}

type countTransport struct{ calls int }

func (c *countTransport) RoundTrip(*http.Request) (*http.Response, error) {
	c.calls++
	return nil, fmt.Errorf("unexpected backend call")
}

func BenchmarkMarshalWithStream(b *testing.B) {
	for _, tc := range []struct {
		name  string
		size  int
		image bool
	}{
		{"short", 64, false},
		{"context256KiB", 256 << 10, false},
		{"image1MiB", 1 << 20, true},
	} {
		b.Run(tc.name, func(b *testing.B) {
			content := ir.Content{Text: strings.Repeat("a", tc.size), Present: true}
			if tc.image {
				content = ir.Content{IsParts: true, Present: true, Parts: []ir.ContentPart{
					{Type: "image_url", ImageURL: &ir.ImageURL{URL: "data:image/jpeg;base64," + strings.Repeat("A", tc.size)}},
				}}
			}
			req := &ir.ChatRequest{Model: "model", Messages: []ir.Message{{Role: ir.RoleUser, Content: content}},
				StreamOptions: &ir.StreamOptions{IncludeUsage: true}}
			b.ReportAllocs()
			b.SetBytes(int64(tc.size))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := marshalWithStream(req, true); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
