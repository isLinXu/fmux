// Package openai implements the shared adapter for any OpenAI-compatible
// server. Every concrete backend (Ollama, llama.cpp, vLLM, ...) embeds it and
// overrides only the parts that genuinely differ.
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/isLinXu/fmux/internal/adapter"
	"github.com/isLinXu/fmux/internal/ir"
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
	cp := *req
	if !wantUsage {
		cp.StreamOptions = nil
	}
	body, err := marshalWithStream(&cp, true)
	if err != nil {
		return nil, err
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

// marshalWithStream serializes a request with an explicit stream flag.
func marshalWithStream(req *ir.ChatRequest, stream bool) ([]byte, error) {
	var options *ir.StreamOptions
	if stream {
		options = req.StreamOptions
	}
	// Explicit fields override the embedded request without mutating it.
	return json.Marshal(struct {
		*ir.ChatRequest
		Stream        bool              `json:"stream"`
		StreamOptions *ir.StreamOptions `json:"stream_options,omitempty"`
	}{req, stream, options})
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
