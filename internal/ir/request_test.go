package ir

import (
	"encoding/json"
	"reflect"
	"sync"
	"testing"
)

func populatedRequest() *ChatRequest {
	temp, topP, presence, frequency := 0.2, 0.8, 0.1, 0.3
	topK, max, maxCompletion, index := 4, 100, 200, 0
	seed := int64(7)
	return &ChatRequest{
		Model: "model",
		Messages: []Message{{Role: RoleUser, Content: Content{IsParts: true, Present: true,
			Parts: []ContentPart{
				{Type: "text", Text: "hello"},
				{Type: "image_url", ImageURL: &ImageURL{URL: "data:image/jpeg;base64,AAAA", Detail: "auto"}},
			}},
			ToolCalls: []ToolCall{{Index: &index, Function: FunctionCall{Name: "tool", Arguments: "{}"}}},
		}},
		Stream: true, StreamOptions: &StreamOptions{IncludeUsage: true},
		Temperature: &temp, TopP: &topP, TopK: &topK, MaxTokens: &max,
		MaxCompletionTok: &maxCompletion, PresencePenalty: &presence,
		FrequencyPenalty: &frequency, Seed: &seed, Stop: []string{"stop"},
		Tools: []Tool{{Type: "function", Function: ToolFuncDef{Name: "tool",
			Parameters: json.RawMessage(`{"type":"object"}`)}}},
		ToolChoice: json.RawMessage(`"auto"`),
		ResponseFormat: &ResponseFormat{Type: "json_schema", JSONSchema: &JSONSchemaSpec{
			Name: "result", Description: "test", Strict: true, Schema: json.RawMessage(`{"type":"object"}`),
		}},
		ResolvedAlias: "alias", ResolvedBackend: "backend", ResolvedModel: "resolved",
		ClientIsFm: true, Quirks: []string{"quirk"},
	}
}

func TestCloneIsolatesMutableFields(t *testing.T) {
	mutations := map[string]func(*ChatRequest){
		"message":      func(r *ChatRequest) { r.Messages[0].Role = RoleAssistant },
		"part":         func(r *ChatRequest) { r.Messages[0].Content.Parts[0].Text = "changed" },
		"image":        func(r *ChatRequest) { r.Messages[0].Content.Parts[1].ImageURL.URL = "changed" },
		"imageDetail":  func(r *ChatRequest) { r.Messages[0].Content.Parts[1].ImageURL.Detail = "changed" },
		"toolCall":     func(r *ChatRequest) { r.Messages[0].ToolCalls[0].Function.Arguments = "changed" },
		"toolIndex":    func(r *ChatRequest) { *r.Messages[0].ToolCalls[0].Index = 42 },
		"stream":       func(r *ChatRequest) { r.StreamOptions.IncludeUsage = false },
		"temperature":  func(r *ChatRequest) { *r.Temperature = 1 },
		"topP":         func(r *ChatRequest) { *r.TopP = 1 },
		"topK":         func(r *ChatRequest) { *r.TopK = 1 },
		"maxTokens":    func(r *ChatRequest) { *r.MaxTokens = 1 },
		"maxComp":      func(r *ChatRequest) { *r.MaxCompletionTok = 1 },
		"presence":     func(r *ChatRequest) { *r.PresencePenalty = 1 },
		"frequency":    func(r *ChatRequest) { *r.FrequencyPenalty = 1 },
		"seed":         func(r *ChatRequest) { *r.Seed = 1 },
		"stop":         func(r *ChatRequest) { r.Stop[0] = "changed" },
		"tool":         func(r *ChatRequest) { r.Tools[0].Function.Name = "changed" },
		"parameters":   func(r *ChatRequest) { r.Tools[0].Function.Parameters[2] = 'X' },
		"toolChoice":   func(r *ChatRequest) { r.ToolChoice[1] = 'X' },
		"format":       func(r *ChatRequest) { r.ResponseFormat.Type = "changed" },
		"schemaName":   func(r *ChatRequest) { r.ResponseFormat.JSONSchema.Name = "changed" },
		"schemaStrict": func(r *ChatRequest) { r.ResponseFormat.JSONSchema.Strict = false },
		"schema":       func(r *ChatRequest) { r.ResponseFormat.JSONSchema.Schema[2] = 'X' },
		"quirk":        func(r *ChatRequest) { r.Quirks[0] = "changed" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			r, want := populatedRequest(), populatedRequest()
			other, cp := r.Clone(), r.Clone()
			if !reflect.DeepEqual(cp, want) {
				t.Fatal("clone did not preserve original values")
			}
			mutate(cp)
			if reflect.DeepEqual(cp, want) {
				t.Fatal("mutation did not exercise field")
			}
			if !reflect.DeepEqual(r, want) {
				t.Error("clone mutation changed original request")
			}
			if !reflect.DeepEqual(other, want) {
				t.Error("clone mutation changed another clone")
			}
		})
	}
}

func TestClonePreservesEmptyShapes(t *testing.T) {
	for _, r := range []*ChatRequest{
		{},
		{Messages: []Message{}, Tools: []Tool{}, Stop: []string{}, ToolChoice: json.RawMessage{}, Quirks: []string{}},
		{Messages: []Message{{Content: Content{Parts: []ContentPart{}, IsParts: true}, ToolCalls: []ToolCall{}}},
			Tools:          []Tool{{Function: ToolFuncDef{Parameters: json.RawMessage{}}}},
			ResponseFormat: &ResponseFormat{JSONSchema: &JSONSchemaSpec{Schema: json.RawMessage{}}}},
		populatedRequest(),
	} {
		if got := r.Clone(); !reflect.DeepEqual(got, r) {
			t.Errorf("clone changed nil/empty shapes or bookkeeping:\ngot=%#v\nwant=%#v", got, r)
		}
	}
}

func TestCloneConcurrentIsolation(t *testing.T) {
	r, want := populatedRequest(), populatedRequest()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		cp := r.Clone()
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				cp.Messages[0].Content.Parts[1].ImageURL.URL = "changed"
				cp.Tools[0].Function.Parameters[2] = 'X'
				cp.ResponseFormat.JSONSchema.Schema[2] = 'X'
				*cp.Temperature = 1
			}
		}()
	}
	wg.Wait()
	if !reflect.DeepEqual(r, want) {
		t.Fatal("concurrent clones modified original")
	}
}
