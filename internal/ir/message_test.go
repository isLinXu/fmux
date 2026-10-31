package ir

import (
	"encoding/json"
	"testing"
)

func TestContentPolymorphism(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantTxt string
		isParts bool
		present bool
		images  int
	}{
		{"string", `"hello"`, "hello", false, true, 0},
		{"array text", `[{"type":"text","text":"a"},{"type":"text","text":"b"}]`, "ab", true, true, 0},
		{"array image", `[{"type":"image_url","image_url":{"url":"data:image/jpeg;base64,AA"}}]`, "", true, true, 1},
		{"null", `null`, "", false, false, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var c Content
			if err := json.Unmarshal([]byte(tc.raw), &c); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got := c.PlainText(); got != tc.wantTxt {
				t.Errorf("PlainText() = %q, want %q", got, tc.wantTxt)
			}
			if c.IsParts != tc.isParts {
				t.Errorf("IsParts = %v, want %v", c.IsParts, tc.isParts)
			}
			if c.Present != tc.present {
				t.Errorf("Present = %v, want %v", c.Present, tc.present)
			}
			if got := c.ImageCount(); got != tc.images {
				t.Errorf("ImageCount() = %d, want %d", got, tc.images)
			}
		})
	}
}

// TestMessageWithoutContentField is the regression guard for fm's `--tool`
// output: {"role":"system"} with no content key at all.
func TestMessageWithoutContentField(t *testing.T) {
	var m Message
	if err := json.Unmarshal([]byte(`{"role":"system"}`), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m.Content.Present {
		t.Error("Content.Present should be false when the key is absent")
	}
	if !m.Content.IsEmpty() {
		t.Error("Content should be empty")
	}
	if m.Role != RoleSystem {
		t.Errorf("Role = %q", m.Role)
	}
}

func TestMessageRoundTrip(t *testing.T) {
	raw := `{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AA"}}]}`
	var m Message
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var a, b map[string]any
	_ = json.Unmarshal([]byte(raw), &a)
	_ = json.Unmarshal(out, &b)
	if len(a) != len(b) {
		t.Fatalf("round trip lost fields: %s", out)
	}
}

func TestRequestValidate(t *testing.T) {
	r := &ChatRequest{Model: "m", Messages: []Message{{Role: RoleUser}}}
	if err := r.Validate(); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}
	if err := (&ChatRequest{Messages: r.Messages}).Validate(); err == nil {
		t.Error("missing model should fail")
	}
	if err := (&ChatRequest{Model: "m"}).Validate(); err == nil {
		t.Error("empty messages should fail")
	}
}

func TestWantsJSONSchema(t *testing.T) {
	r := &ChatRequest{ResponseFormat: &ResponseFormat{
		Type:       "json_schema",
		JSONSchema: &JSONSchemaSpec{Name: "P", Strict: true},
	}}
	if !r.WantsJSONSchema() {
		t.Error("expected WantsJSONSchema")
	}
	r.ResponseFormat = &ResponseFormat{Type: "text"}
	if r.WantsJSONSchema() {
		t.Error("text format should not want json_schema")
	}
}
