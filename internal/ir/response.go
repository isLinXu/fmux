package ir

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

// Usage is the token accounting block.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ChatResponse is a non-streaming completion.
type ChatResponse struct {
	ID                string           `json:"id"`
	Object            string           `json:"object"`
	Created           int64            `json:"created"`
	Model             string           `json:"model"`
	Choices           []ResponseChoice `json:"choices"`
	Usage             *Usage           `json:"usage,omitempty"`
	SystemFingerprint string           `json:"system_fingerprint,omitempty"`
}

// Chunk is one SSE payload of a streaming completion.
type Chunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []ChunkChoice `json:"choices"`
	Usage   *Usage        `json:"usage,omitempty"`
}

// ModelCapabilities is what a model can actually do.
//
// Without this a client has no way to know: it finds out by sending a request
// and getting a 400 back. The same `fm respond --schema` works against Ollama
// and fails against MLX, and nothing in /v1/models said so.
type ModelCapabilities struct {
	Stream     bool `json:"stream"`
	Vision     bool `json:"vision"`
	JSONSchema bool `json:"json_schema"`
	Tools      bool `json:"tools"`
}

// ModelInfo describes one model exposed through /v1/models.
type ModelInfo struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by,omitempty"`

	// Extra metadata. Non-standard fields are ignored by OpenAI clients, and
	// carrying them beats making people curl /admin/backends to find out what
	// an alias can do.
	Backend      string             `json:"backend,omitempty"`
	ContextLen   int                `json:"context_length,omitempty"`
	Tags         []string           `json:"tags,omitempty"`
	Capabilities *ModelCapabilities `json:"capabilities,omitempty"`
}

// ModelList is the /v1/models envelope.
type ModelList struct {
	Object string      `json:"object"`
	Data   []ModelInfo `json:"data"`
}

// NewID returns an OpenAI-shaped completion id.
func NewID(prefix string) string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return prefix + "-" + hex.EncodeToString(b[:])
}

// Now returns the Unix seconds used in response envelopes.
func Now() int64 { return time.Now().Unix() }

// NewChatResponse builds an empty non-streaming response for a model.
func NewChatResponse(model string) *ChatResponse {
	return &ChatResponse{
		ID:      NewID("chatcmpl"),
		Object:  "chat.completion",
		Created: Now(),
		Model:   model,
	}
}

// NewChunk builds an empty streaming chunk for a model.
func NewChunk(model string) *Chunk {
	return &Chunk{
		ID:      NewID("chatcmpl"),
		Object:  "chat.completion.chunk",
		Created: Now(),
		Model:   model,
		Choices: []ChunkChoice{},
	}
}
