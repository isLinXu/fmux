// Package openai implements the shared adapter for any OpenAI-compatible
// server. Every concrete backend (Ollama, llama.cpp, vLLM, ...) embeds it and
// overrides only the parts that genuinely differ.
package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/linxu/fmux/internal/adapter"
	"github.com/linxu/fmux/internal/ir"
)

// Options configures the adapter.
type Options struct {
	Name    string
	Type    string
	BaseURL string
	APIKey  string
	Headers map[string]string
	Caps    adapter.Capabilities
	Client  *http.Client
}

// Adapter speaks the OpenAI HTTP protocol.
type Adapter struct {
	opts   Options
	client *http.Client
}

// New builds an adapter. BaseURL must include the /v1 suffix.
func New(o Options) *Adapter {
	if o.Client == nil {
		o.Client = &http.Client{Timeout: 0}
	}
	o.BaseURL = strings.TrimRight(o.BaseURL, "/")
	return &Adapter{opts: o, client: o.Client}
}

// Name returns the backend name.
func (a *Adapter) Name() string { return a.opts.Name }

// Type returns the backend type.
func (a *Adapter) Type() string { return a.opts.Type }

// Caps returns the declared capabilities.
func (a *Adapter) Caps() adapter.Capabilities { return a.opts.Caps }

// BaseURL returns the configured endpoint.
func (a *Adapter) BaseURL() string { return a.opts.BaseURL }

func (a *Adapter) url(path string) string {
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		return path
	}
	return a.opts.BaseURL + path
}

func (a *Adapter) newRequest(ctx context.Context, method, path string, body []byte) (*http.Request, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.url(path), rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if a.opts.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+a.opts.APIKey)
	}
	for k, v := range a.opts.Headers {
		req.Header.Set(k, v)
	}
	return req, nil
}

// Models lists the backend's models.
func (a *Adapter) Models(ctx context.Context) ([]ir.ModelInfo, error) {
	req, err := a.newRequest(ctx, http.MethodGet, "/models", nil)
	if err != nil {
		return nil, err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, statusError(resp)
	}
	var list ir.ModelList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, fmt.Errorf("decode model list: %w", err)
	}
	for i := range list.Data {
		list.Data[i].Backend = a.opts.Name
		if list.Data[i].Object == "" {
			list.Data[i].Object = "model"
		}
	}
	return list.Data, nil
}

// Health probes the backend with a short timeout.
func (a *Adapter) Health(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := a.newRequest(ctx, http.MethodGet, "/models", nil)
	if err != nil {
		return err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode/100 != 2 {
		return statusError(resp)
	}
	return nil
}

// Chat performs a non-streaming completion.
func (a *Adapter) Chat(ctx context.Context, req *ir.ChatRequest) (*ir.ChatResponse, error) {
	body, err := marshalWithStream(req, false)
	if err != nil {
		return nil, err
	}
	httpReq, err := a.newRequest(ctx, http.MethodPost, "/chat/completions", body)
	if err != nil {
		return nil, err
	}
	resp, err := a.client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, statusError(resp)
	}
	var out ir.ChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode completion: %w", err)
	}
	if out.ID == "" {
		out.ID = ir.NewID("chatcmpl")
	}
	if out.Object == "" {
		out.Object = "chat.completion"
	}
	if out.Created == 0 {
		out.Created = ir.Now()
	}
	return &out, nil
}

// ChatStream starts a streaming completion.
func (a *Adapter) ChatStream(ctx context.Context, req *ir.ChatRequest) (adapter.Stream, error) {
	wantUsage := req.WantsUsageInStream() && a.opts.Caps.UsageInStream
	body, err := marshalWithStream(req, true)
	if err != nil {
		return nil, err
	}
	if !wantUsage {
		body, err = dropJSONField(body, "stream_options")
		if err != nil {
			return nil, err
		}
	}
	httpReq, err := a.newRequest(ctx, http.MethodPost, "/chat/completions", body)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := a.client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		return nil, statusError(resp)
	}
	return newSSEStream(resp.Body), nil
}

// sseStream parses an OpenAI-style SSE response body.
type sseStream struct {
	body io.ReadCloser
	br   *bufio.Reader
	done bool
}

func newSSEStream(body io.ReadCloser) *sseStream {
	br := bufio.NewReaderSize(body, 1<<20)
	return &sseStream{body: body, br: br}
}

func (s *sseStream) Recv() (*ir.Chunk, error) {
	if s.done {
		return nil, io.EOF
	}
	var payload []byte
	for {
		line, err := s.br.ReadBytes('\n')
		if len(line) > 0 {
			trimmed := bytes.TrimRight(line, "\r\n")
			switch {
			case len(trimmed) == 0:
				if payload != nil {
					return s.decode(payload)
				}
				continue
			case bytes.HasPrefix(trimmed, []byte(":")):
				continue // comment / keep-alive
			case bytes.HasPrefix(trimmed, []byte("data:")):
				v := bytes.TrimPrefix(trimmed, []byte("data:"))
				v = bytes.TrimSpace(v)
				if bytes.Equal(v, []byte("[DONE]")) {
					s.done = true
					return nil, io.EOF
				}
				if payload == nil {
					payload = append([]byte(nil), v...)
				} else {
					payload = append(payload, v...)
				}
			case bytes.HasPrefix(trimmed, []byte("event:")):
				continue
			}
		}
		if err != nil {
			if err == io.EOF {
				s.done = true
				if payload != nil {
					return s.decode(payload)
				}
				return nil, io.EOF
			}
			return nil, err
		}
		// A complete data line is enough for OpenAI-style servers.
		if payload != nil {
			return s.decode(payload)
		}
	}
}

func (s *sseStream) decode(payload []byte) (*ir.Chunk, error) {
	var c ir.Chunk
	if err := json.Unmarshal(payload, &c); err != nil {
		return nil, fmt.Errorf("decode chunk %q: %w", truncate(payload, 200), err)
	}
	if c.Object == "" {
		c.Object = "chat.completion.chunk"
	}
	return &c, nil
}

func (s *sseStream) Close() error { return s.body.Close() }

// marshalWithStream serializes a request with an explicit stream flag.
func marshalWithStream(req *ir.ChatRequest, stream bool) ([]byte, error) {
	b, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	m["stream"] = json.RawMessage(strconv.FormatBool(stream))
	if !stream {
		delete(m, "stream_options")
	}
	return json.Marshal(m)
}

func dropJSONField(body []byte, field string) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	delete(m, field)
	return json.Marshal(m)
}

// statusError turns a non-2xx response into an error carrying the body.
func statusError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return &HTTPError{
		StatusCode: resp.StatusCode,
		Status:     resp.Status,
		Body:       strings.TrimSpace(string(b)),
	}
}

// HTTPError is a backend error with its status code preserved.
type HTTPError struct {
	StatusCode int
	Status     string
	Body       string
}

func (e *HTTPError) Error() string {
	if e.Body == "" {
		return "backend returned " + e.Status
	}
	return fmt.Sprintf("backend returned %s: %s", e.Status, e.Body)
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}
