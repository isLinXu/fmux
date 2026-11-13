package openai

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	"github.com/isLinXu/fmux/internal/ir"
)

// StreamError is an application error delivered inside an HTTP 200 SSE stream.
// It is not an HTTPError: its code does not establish an HTTP status.
type StreamError struct {
	Message string          `json:"message"`
	Type    string          `json:"type"`
	Code    json.RawMessage `json:"code,omitempty"`
	Param   json.RawMessage `json:"param,omitempty"`
}

func (e *StreamError) Error() string { return "backend stream error: " + e.Message }

type sseStream struct {
	body io.ReadCloser
	br   *bufio.Reader
	done bool
}

func newSSEStream(body io.ReadCloser) *sseStream {
	return &sseStream{body: body, br: bufio.NewReaderSize(body, 1<<20)}
}

func (s *sseStream) Recv() (*ir.Chunk, error) {
	if s.done {
		return nil, io.EOF
	}
	var payload []byte
	var hasData bool
	var event string
	for {
		line, err := s.br.ReadBytes('\n')
		line = bytes.TrimSuffix(line, []byte("\n"))
		line = bytes.TrimSuffix(line, []byte("\r"))
		if len(line) == 0 && err == nil {
			if hasData || event == "error" {
				return s.decode(payload, event)
			}
			event = ""
		} else if len(line) > 0 && line[0] != ':' {
			field, value, _ := bytes.Cut(line, []byte(":"))
			value = bytes.TrimPrefix(value, []byte(" "))
			switch string(field) {
			case "data":
				if hasData {
					payload = append(payload, '\n')
				}
				payload = append(payload, value...)
				hasData = true
			case "event":
				event = string(value)
			}
		}
		if err != nil {
			s.done = true
			if err == io.EOF && (hasData || event == "error") {
				// Preserve compatibility with providers omitting the last blank line.
				return s.decode(payload, event)
			}
			return nil, err
		}
	}
}

func (s *sseStream) decode(payload []byte, event string) (*ir.Chunk, error) {
	if event != "error" && bytes.Equal(bytes.TrimSpace(payload), []byte("[DONE]")) {
		s.done = true
		return nil, io.EOF
	}
	var chunk ir.Chunk
	envelope := struct {
		*ir.Chunk
		Error json.RawMessage `json:"error"`
	}{Chunk: &chunk}
	err := json.Unmarshal(payload, &envelope)
	if err == nil && len(envelope.Error) > 0 && !bytes.Equal(envelope.Error, []byte("null")) {
		s.done = true
		var upstream StreamError
		if json.Unmarshal(envelope.Error, &upstream) != nil {
			upstream.Message = truncate(envelope.Error, 200)
		}
		if upstream.Message == "" {
			upstream.Message = "upstream error"
		}
		return nil, &upstream
	}
	if event == "error" {
		s.done = true
		return nil, &StreamError{Message: "upstream error event: " + truncate(payload, 200)}
	}
	if err != nil {
		s.done = true
		return nil, fmt.Errorf("decode chunk %q: %w", truncate(payload, 200), err)
	}
	if chunk.Object == "" {
		chunk.Object = "chat.completion.chunk"
	}
	return &chunk, nil
}

func (s *sseStream) Close() error { return s.body.Close() }
