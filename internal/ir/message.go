package ir

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Role is an OpenAI chat role.
type Role string

const (
	RoleSystem    Role = "system"
	RoleDeveloper Role = "developer"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// ContentPart is one element of a multimodal content array.
type ContentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
}

// ImageURL carries either a remote URL or a data: URL.
type ImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

// Content models the polymorphic `content` field: either a plain string or an
// array of parts. It also tracks whether the field was present at all, because
// `fm --tool` emits {"role":"system"} with no content key whatsoever.
type Content struct {
	Text    string
	Parts   []ContentPart
	IsParts bool
	Present bool
}

// UnmarshalJSON accepts string, array, or absent/null.
func (c *Content) UnmarshalJSON(b []byte) error {
	trimmed := strings.TrimSpace(string(b))
	if trimmed == "" || trimmed == "null" {
		*c = Content{}
		return nil
	}
	switch b[0] {
	case '"':
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*c = Content{Text: s, Present: true}
		return nil
	case '[':
		var parts []ContentPart
		if err := json.Unmarshal(b, &parts); err != nil {
			return err
		}
		*c = Content{Parts: parts, IsParts: true, Present: true}
		return nil
	default:
		return fmt.Errorf("ir: unsupported content type %q", trimmed[:1])
	}
}

// MarshalJSON round-trips the original shape.
func (c Content) MarshalJSON() ([]byte, error) {
	if c.IsParts {
		if c.Parts == nil {
			return []byte("[]"), nil
		}
		return json.Marshal(c.Parts)
	}
	return json.Marshal(c.Text)
}

// IsEmpty reports whether the content carries no information.
func (c Content) IsEmpty() bool {
	if c.IsParts {
		for _, p := range c.Parts {
			if p.Type == "text" && p.Text != "" {
				return false
			}
			if p.ImageURL != nil && p.ImageURL.URL != "" {
				return false
			}
		}
		return true
	}
	return c.Text == ""
}

// PlainText flattens text parts into a single string (images are dropped).
func (c Content) PlainText() string {
	if !c.IsParts {
		return c.Text
	}
	var sb strings.Builder
	for _, p := range c.Parts {
		if p.Type == "text" {
			sb.WriteString(p.Text)
		}
	}
	return sb.String()
}

// ImageCount returns how many image parts the content holds.
func (c Content) ImageCount() int {
	n := 0
	for _, p := range c.Parts {
		if p.ImageURL != nil && p.ImageURL.URL != "" {
			n++
		}
	}
	return n
}

// Message is a single chat message.
type Message struct {
	Role       Role       `json:"role"`
	Content    Content    `json:"content,omitempty"`
	Name       string     `json:"name,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// ToolCall is an assistant-emitted function call.
type ToolCall struct {
	Index    *int         `json:"index,omitempty"`
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"`
	Function FunctionCall `json:"function"`
}

// FunctionCall is the name/arguments pair inside a ToolCall.
type FunctionCall struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// Tool describes a callable function.
type Tool struct {
	Type     string      `json:"type"`
	Function ToolFuncDef `json:"function"`
}

// ToolFuncDef is the schema of a callable function.
type ToolFuncDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// Delta is the incremental payload inside a streaming chunk.
type Delta struct {
	Role      string     `json:"role,omitempty"`
	Content   string     `json:"content,omitempty"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

// ResponseChoice is a non-streaming choice.
type ResponseChoice struct {
	Index        int             `json:"index"`
	Message      Message         `json:"message"`
	FinishReason *string         `json:"finish_reason"`
	Logprobs     json.RawMessage `json:"logprobs,omitempty"`
}

// ChunkChoice is a streaming choice.
type ChunkChoice struct {
	Index        int             `json:"index"`
	Delta        Delta           `json:"delta"`
	FinishReason *string         `json:"finish_reason"`
	Logprobs     json.RawMessage `json:"logprobs,omitempty"`
}
