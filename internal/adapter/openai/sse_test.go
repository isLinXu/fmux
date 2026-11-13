package openai

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func TestSSEErrors(t *testing.T) {
	for _, tc := range []struct {
		name, wire, message string
	}{
		{"envelope", `data: {"error":{"message":"injected failure","type":"server_error","code":"injected"}}`, "injected failure"},
		{"event", "event: error\n" + `data: {"error":{"message":"event failure"}}`, "event failure"},
		{"numeric", `data: {"error":{"message":"numeric failure","code":42,"param":null}}`, "numeric failure"},
		{"null", `data: {"error":{"message":"null failure","code":null}}`, "null failure"},
		{"plain", "event: error\ndata: plain failure", "plain failure"},
		{"non_envelope", "event: error\ndata: " + `{"message":"flat failure"}`, "flat failure"},
		{"no_data", "event: error", "error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newSSEStream(io.NopCloser(strings.NewReader(tc.wire + "\n\n" +
				`data: {"choices":[{"delta":{"content":"must not arrive"}}]}` + "\n\n")))
			ch, err := st.Recv()
			if err == nil || ch != nil || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("chunk=%+v err=%v want error containing %q", ch, err, tc.message)
			}
			if ch, err := st.Recv(); ch != nil || !errors.Is(err, io.EOF) {
				t.Fatalf("error must terminate stream: chunk=%+v err=%v", ch, err)
			}
		})
	}
}

func TestSSEEvents(t *testing.T) {
	const content = `{"id":"one","choices":[{"index":0,"delta":{"content":"中文\nsecond"}}]}`
	for _, tc := range []struct {
		name, wire, want string
		fragment         int
	}{
		{"single", "data: " + content + "\n\n", "中文\nsecond", 0},
		{"crlf_comments", ":keepalive\r\nid: ignored\r\nevent: message\r\ndata: " + content + "\r\n\r\n", "中文\nsecond", 0},
		{"multiline", "data: {\"id\":\"one\",\ndata: \"choices\":[{\"index\":0,\"delta\":{\"content\":\"multiline\"}}]}\n\n", "multiline", 0},
		{"bare_data", "data\ndata: " + content + "\n\n", "中文\nsecond", 0},
		{"eof", "data: " + content, "中文\nsecond", 0},
		{"fragmented", "data: " + content + "\n\n", "中文\nsecond", 1},
		{"large", `data: {"choices":[{"delta":{"content":"` + strings.Repeat("z", 70000) + `🙂"}}]}` + "\n\n", strings.Repeat("z", 70000) + "🙂", 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var reader io.Reader = strings.NewReader(tc.wire + "\ndata: [DONE]\n\n")
			if tc.name == "eof" {
				reader = strings.NewReader(tc.wire)
			}
			if tc.fragment > 0 {
				reader = &fragmentReader{reader, tc.fragment}
			}
			st := newSSEStream(io.NopCloser(reader))
			ch, err := st.Recv()
			if err != nil || ch == nil {
				t.Fatalf("chunk=%+v err=%v", ch, err)
			}
			if len(ch.Choices) != 1 || ch.Choices[0].Delta.Content != tc.want {
				t.Fatalf("unexpected choices: %+v", ch.Choices)
			}
			for i := 0; i < 2; i++ {
				if ch, err := st.Recv(); ch != nil || !errors.Is(err, io.EOF) {
					t.Fatalf("after completion: chunk=%+v err=%v", ch, err)
				}
			}
		})
	}
}

func TestSSEEmptyAndMalformed(t *testing.T) {
	for _, wire := range []string{"data:\n\n", "data\n\n", "data: {\n\n"} {
		st := newSSEStream(io.NopCloser(strings.NewReader(wire)))
		if ch, err := st.Recv(); ch != nil || err == nil {
			t.Fatalf("wire=%q chunk=%+v err=%v", wire, ch, err)
		}
	}
	for _, wire := range []string{"", ": comment\n\n", "event: message\n\n", "data: [DONE]\n\n"} {
		st := newSSEStream(io.NopCloser(strings.NewReader(wire)))
		if ch, err := st.Recv(); ch != nil || !errors.Is(err, io.EOF) {
			t.Fatalf("wire=%q chunk=%+v err=%v", wire, ch, err)
		}
	}
}

type fragmentReader struct {
	io.Reader
	size int
}

func (r *fragmentReader) Read(p []byte) (int, error) {
	if len(p) > r.size {
		p = p[:r.size]
	}
	return r.Reader.Read(p)
}
