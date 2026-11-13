package api

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/isLinXu/fmux/internal/adapter"
	"github.com/isLinXu/fmux/internal/adapter/mock"
	"github.com/isLinXu/fmux/internal/config"
	"github.com/isLinXu/fmux/internal/ir"
)

type mutatingAdapter struct{ adapter.Adapter }

func (a mutatingAdapter) Chat(ctx context.Context, req *ir.ChatRequest) (*ir.ChatResponse, error) {
	req.Messages[0].Content.Parts[0].Text = "changed"
	req.Messages[0].Content.Parts[1].ImageURL.URL = "changed"
	req.Tools[0].Function.Parameters[2] = 'X'
	req.ResponseFormat.JSONSchema.Schema[2] = 'X'
	req.Stop[0] = "changed"
	*req.Temperature = 1
	return nil, fmt.Errorf("backend failed after modifying its request")
}

func TestFallbackRequestIsolation(t *testing.T) {
	first, second := mock.New("first"), mock.New("second")
	reg := adapter.NewRegistry()
	reg.Add(mutatingAdapter{first})
	reg.Add(second)
	cfg := &config.Config{
		Backends: []config.Backend{{Name: "first", Type: "mock"}, {Name: "second", Type: "mock"}},
		Aliases: map[string]config.Alias{
			"auto":   {Backend: "first", Model: "m", Fallback: []string{"backup"}},
			"backup": {Backend: "second", Model: "m"},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	s := New(Options{Config: cfg, Registry: reg, Log: testLogger()})
	rec := doChat(t, s, `{"model":"auto","temperature":0.2,"stop":["stop"],
		"messages":[{"role":"user","content":[{"type":"text","text":"hello"},
		{"type":"image_url","image_url":{"url":"data:image/jpeg;base64,AAAA"}}]}],
		"tools":[{"type":"function","function":{"name":"tool","parameters":{"type":"object"}}}],
		"response_format":{"type":"json_schema","json_schema":{"name":"result","schema":{"type":"object"}}}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("fallback failed: %s", rec.Body.String())
	}
	got := second.LastRequest()
	if got == nil {
		t.Fatal("fallback not called")
	}
	if got.Messages[0].Content.Parts[0].Text != "hello" ||
		got.Messages[0].Content.Parts[1].ImageURL.URL != "data:image/jpeg;base64,AAAA" ||
		string(got.Tools[0].Function.Parameters) != `{"type":"object"}` ||
		string(got.ResponseFormat.JSONSchema.Schema) != `{"type":"object"}` ||
		got.Stop[0] != "stop" || *got.Temperature != 0.2 {
		t.Fatalf("fallback received mutated fields: %+v", got)
	}
}
