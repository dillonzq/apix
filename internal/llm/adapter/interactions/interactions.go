// Package interactions validates the native Gemini Interactions path.
// It intentionally has no cross-protocol conversion or session storage.
package interactions

import (
	"encoding/json"
	"strings"

	"github.com/dillonzq/apix/internal/errclass"
	"github.com/dillonzq/apix/internal/llm/adapter/shared"
)

const Format = "interactions"

func ConvertResponse(status int, body []byte) ([]byte, *errclass.Error) {
	if status >= 400 {
		return nil, shared.UpstreamStatusError(status, body)
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil || root == nil {
		return nil, errclass.Translation("malformed Interactions response")
	}
	if shared.HasContent(root["error"]) {
		return nil, shared.UpstreamStatusError(502, body)
	}
	var id, state string
	if json.Unmarshal(root["id"], &id) != nil || id == "" {
		return nil, errclass.Translation("Interactions response requires an id")
	}
	if json.Unmarshal(root["status"], &state) != nil || state == "" {
		return nil, errclass.Translation("Interactions response requires a status")
	}
	if state == "failed" {
		return nil, errclass.UpstreamFallback("Interactions execution failed")
	}
	return body, nil
}

type StreamConverter struct {
	framer         *shared.SSEFramer
	terminal, done bool
	failure        *errclass.Error
}

func NewStreamConverter() *StreamConverter {
	return &StreamConverter{framer: shared.NewSSEFramer(true)}
}
func (s *StreamConverter) Feed(chunk []byte) ([][]byte, bool, *errclass.Error) {
	if s.failure != nil {
		return nil, false, s.failure
	}
	if s.done {
		return nil, true, nil
	}
	s.framer.Push(chunk)
	out, err := s.drain()
	s.failure = err
	return out, s.done && err == nil, err
}
func (s *StreamConverter) drain() ([][]byte, *errclass.Error) {
	var out [][]byte
	for {
		name, data, raw, ok := s.framer.Next()
		if !ok {
			return out, nil
		}
		if data == "" {
			out = append(out, raw)
			continue
		}
		if strings.TrimSpace(data) == "[DONE]" {
			if !s.terminal {
				return out, errclass.Translation("Interactions done before terminal state")
			}
			s.done = true
			return append(out, raw), nil
		}
		var root struct {
			EventType   string          `json:"event_type"`
			Type        string          `json:"type"`
			Error       json.RawMessage `json:"error"`
			Interaction struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			} `json:"interaction"`
		}
		if json.Unmarshal([]byte(data), &root) != nil || !shared.IsJSONObject(data) {
			return out, errclass.Translation("malformed Interactions SSE event")
		}
		typ := root.EventType
		if typ == "" {
			typ = root.Type
		}
		if typ == "" {
			typ = name
		}
		switch typ {
		case "interaction.failed", "error", "response.failed":
			return out, errclass.UpstreamFallback("Interactions stream failed")
		case "interaction.completed":
			if root.Interaction.ID == "" || root.Interaction.Status != "completed" {
				return out, errclass.Translation("invalid Interactions completion")
			}
			s.terminal = true
		case "interaction.requires_action":
			if root.Interaction.ID == "" {
				return out, errclass.Translation("invalid Interactions tool handoff")
			}
			s.terminal = true
		case "done":
			if !s.terminal {
				return out, errclass.Translation("Interactions done before terminal state")
			}
			s.done = true
		}
		if shared.HasContent(root.Error) {
			return out, errclass.UpstreamFallback("Interactions stream error")
		}
		out = append(out, raw)
		if s.done {
			return out, nil
		}
	}
}
func (s *StreamConverter) Finish() ([][]byte, *errclass.Error) {
	if s.failure != nil {
		return nil, s.failure
	}
	if s.done {
		return nil, nil
	}
	s.framer.End()
	out, err := s.drain()
	if err == nil && !s.terminal {
		err = errclass.Translation("Interactions stream ended without a terminal state")
	}
	if err != nil {
		s.failure = err
	} else {
		s.done = true
	}
	return out, err
}
