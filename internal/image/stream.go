package image

import (
	"encoding/json"
	"strings"

	"github.com/dillonzq/apix"
	"github.com/dillonzq/apix/internal/vendorerr"
	"github.com/dillonzq/apix/sse"
)

// NewStreamConverter validates a stream from an upstream that speaks the
// output protocol and passes its events through. Other exchanges never ask
// the upstream to stream; they emulate streams in ConvertResponse.
func (e *exchange) NewStreamConverter() (apix.StreamConverter, error) {
	if !e.upstreamStream {
		return nil, apix.Unsupported(string(e.upstream) + " was not asked to stream for " + string(e.output))
	}
	s := &passthrough{framer: sse.NewFramer(true)}
	if protocols[e.upstream].family == familyGemini {
		s.check = checkGeminiEvent
	} else {
		s.check = openaiChecker(eventPrefix(e.upstream), e.expected)
	}
	return s, nil
}

// eventCheck validates one event's data. It reports whether the event ends
// the stream (done) and whether the stream may end after it (terminal).
type eventCheck func(name, data string) (done, terminal bool, err *apix.Error)

// passthrough emits validated upstream SSE frames verbatim.
type passthrough struct {
	framer   *sse.Framer
	check    eventCheck
	terminal bool
	done     bool
	err      *apix.Error
}

func (s *passthrough) Feed(chunk []byte) ([][]byte, bool, error) {
	if s.err != nil {
		return nil, false, s.err
	}
	if s.done {
		return nil, true, nil
	}
	s.framer.Push(chunk)
	events := s.drain()
	if s.err != nil {
		return events, false, s.err
	}
	return events, s.done, nil
}

func (s *passthrough) Finish() ([][]byte, error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.done {
		return nil, nil
	}
	s.framer.End()
	events := s.drain()
	if s.err == nil && !s.terminal {
		s.err = apix.Translation("image stream ended without a completed image")
	}
	if s.err != nil {
		return events, s.err
	}
	return events, nil
}

func (s *passthrough) drain() [][]byte {
	var events [][]byte
	for !s.done {
		name, data, raw, ok := s.framer.Next()
		if !ok {
			break
		}
		done, terminal, err := s.check(name, data)
		if err != nil {
			s.err = err
			break
		}
		events = append(events, raw)
		s.terminal = s.terminal || terminal
		s.done = done
	}
	return events
}

// streamError classifies an error object an upstream sent inside a stream.
func streamError(data string) *apix.Error {
	msg := vendorerr.Snippet(strings.TrimSpace(data))
	var probe struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(data), &probe) == nil && probe.Error.Message != "" {
		msg = vendorerr.Snippet(probe.Error.Message)
	}
	return apix.UpstreamFallback("upstream stream error: " + msg)
}

// openaiChecker validates generation or edit events. The stream is done once
// the expected number of images completed; fewer are accepted at EOF.
func openaiChecker(prefix string, expected int) eventCheck {
	completed := 0
	return func(name, data string) (bool, bool, *apix.Error) {
		// Keys match exactly: clients read b64_json, not a case variant
		// encoding/json would accept.
		var ev map[string]json.RawMessage
		if json.Unmarshal([]byte(data), &ev) != nil || ev == nil {
			return false, false, apix.Translation("malformed OpenAI image stream event")
		}
		var typ string
		if ev["type"] != nil && json.Unmarshal(ev["type"], &typ) != nil {
			return false, false, apix.Translation("OpenAI image stream event type must be a string")
		}
		if typ == "" {
			typ = name
		}
		switch {
		case typ == "error" || name == "error" || ev["error"] != nil:
			return false, false, streamError(data)
		case typ == prefix+".completed":
			var b64 string
			if json.Unmarshal(ev["b64_json"], &b64) != nil || b64 == "" {
				return false, false, apix.Translation("OpenAI image stream completed event has no b64_json")
			}
			completed++
			return completed >= expected, true, nil
		}
		return false, false, nil
	}
}

// checkGeminiEvent validates a GenerateContent chunk. A finish reason or a
// blocked prompt makes the stream complete; Gemini ends it at EOF.
func checkGeminiEvent(_, data string) (bool, bool, *apix.Error) {
	var probe struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal([]byte(data), &probe) == nil && probe.Error != nil {
		return false, false, streamError(data)
	}
	r, err := decodeGeminiResponse([]byte(data))
	if err != nil {
		return false, false, err
	}
	terminal := r.PromptFeedback != nil && r.PromptFeedback.BlockReason != ""
	for _, c := range r.Candidates {
		terminal = terminal || c.FinishReason != ""
	}
	return false, terminal, nil
}
