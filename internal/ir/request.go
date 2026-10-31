package ir

import (
	"encoding/json"
	"fmt"
)

// StreamOptions controls streaming extras.
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

// JSONSchemaSpec is the inner object of a json_schema response format.
type JSONSchemaSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Schema      json.RawMessage `json:"schema,omitempty"`
	Strict      bool            `json:"strict,omitempty"`
}

// ResponseFormat mirrors OpenAI's response_format.
type ResponseFormat struct {
	Type       string          `json:"type"`
	JSONSchema *JSONSchemaSpec `json:"json_schema,omitempty"`
}

// ChatRequest is the internal representation of a chat completion request.
// It covers the union of fields fmux accepts and forwards.
type ChatRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`

	Stream        bool           `json:"stream,omitempty"`
	StreamOptions *StreamOptions `json:"stream_options,omitempty"`

	Temperature      *float64 `json:"temperature,omitempty"`
	TopP             *float64 `json:"top_p,omitempty"`
	TopK             *int     `json:"top_k,omitempty"`
	MaxTokens        *int     `json:"max_tokens,omitempty"`
	MaxCompletionTok *int     `json:"max_completion_tokens,omitempty"`
	PresencePenalty  *float64 `json:"presence_penalty,omitempty"`
	FrequencyPenalty *float64 `json:"frequency_penalty,omitempty"`
	Seed             *int64   `json:"seed,omitempty"`
	Stop             []string `json:"stop,omitempty"`

	ResponseFormat *ResponseFormat `json:"response_format,omitempty"`
	Tools          []Tool          `json:"tools,omitempty"`
	ToolChoice     json.RawMessage `json:"tool_choice,omitempty"`

	// Non-wire bookkeeping, filled in by the router. Not serialized.
	ResolvedAlias   string   `json:"-"`
	ResolvedBackend string   `json:"-"`
	ResolvedModel   string   `json:"-"`
	ClientIsFm      bool     `json:"-"`
	Quirks          []string `json:"-"`
}

// Validate performs the minimum sanity checks the HTTP layer needs.
func (r *ChatRequest) Validate() error {
	if r.Model == "" {
		return fmt.Errorf("model is required")
	}
	if len(r.Messages) == 0 {
		return fmt.Errorf("messages must not be empty")
	}
	for i, m := range r.Messages {
		if m.Role == "" {
			return fmt.Errorf("messages[%d]: role is required", i)
		}
	}
	return nil
}

// EffectiveMaxTokens returns whichever token cap was supplied.
func (r *ChatRequest) EffectiveMaxTokens() *int {
	if r.MaxCompletionTok != nil {
		return r.MaxCompletionTok
	}
	return r.MaxTokens
}

// HasImages reports whether any message carries an image part.
func (r *ChatRequest) HasImages() bool {
	for _, m := range r.Messages {
		if m.Content.ImageCount() > 0 {
			return true
		}
	}
	return false
}

// WantsJSONSchema reports whether a strict json_schema output was requested.
func (r *ChatRequest) WantsJSONSchema() bool {
	return r.ResponseFormat != nil &&
		r.ResponseFormat.Type == "json_schema" &&
		r.ResponseFormat.JSONSchema != nil
}

// WantsUsageInStream reports whether the client asked for a final usage chunk.
func (r *ChatRequest) WantsUsageInStream() bool {
	return r.StreamOptions != nil && r.StreamOptions.IncludeUsage
}

// Clone returns a shallow copy safe for adapter-side mutation.
func (r *ChatRequest) Clone() *ChatRequest {
	cp := *r
	cp.Messages = append([]Message(nil), r.Messages...)
	cp.Quirks = append([]string(nil), r.Quirks...)
	if r.Tools != nil {
		cp.Tools = append([]Tool(nil), r.Tools...)
	}
	if r.StreamOptions != nil {
		so := *r.StreamOptions
		cp.StreamOptions = &so
	}
	return &cp
}
