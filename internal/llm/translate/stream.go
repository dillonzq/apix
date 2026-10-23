package translate

import (
	"bytes"
	"encoding/json"

	"github.com/dillonzq/apix/internal/llm/adapter/chatcompletions"
	"github.com/dillonzq/apix/internal/llm/adapter/gemini"
	"github.com/dillonzq/apix/internal/llm/adapter/messages"
	"github.com/dillonzq/apix/internal/llm/adapter/responses"
	"github.com/dillonzq/apix/internal/llm/adapter/shared"
)

// StreamConverter accepts arbitrary SSE byte chunks and returns encoded SSE
// events. Each converter belongs to one execution and is not concurrency safe.
// Always call Finish on EOF; a stream without a valid terminal state fails.
type StreamConverter interface {
	Feed(chunk []byte) (events [][]byte, done bool, err *Error)
	Finish() (events [][]byte, err *Error)
}

// NewStreamConverter converts upstreamFormat SSE into outputFormat SSE. State
// is the state populated by ConvertRequest; nil is valid without tool mappings.
func NewStreamConverter(upstreamFormat, outputFormat string, state *RequestState) (StreamConverter, *Error) {
	if err := validateFormats(upstreamFormat, outputFormat); err != nil {
		return nil, err
	}
	if upstreamFormat == FormatGemini || outputFormat == FormatGemini {
		converted := gemini.NewStreamConverter(upstreamFormat, outputFormat, conversionContext(state))
		if upstreamFormat == FormatGemini {
			return converted, nil
		}
		// Reuse source validation and complete source frames. Conversion remains
		// direct; validation must run before a target terminal can be emitted.
		validator, err := NewStreamConverter(upstreamFormat, upstreamFormat, nil)
		if err != nil {
			return nil, err
		}
		return &validatedGeminiStream{converted: converted, validator: validator}, nil
	}
	var converter StreamConverter
	switch upstreamFormat {
	case FormatChatCompletions:
		converter = chatcompletions.NewStreamConverter(outputFormat, conversionContext(state))
	case FormatMessages:
		converter = messages.NewStreamConverter(outputFormat, conversionContext(state))
	default:
		converter = responses.NewStreamConverter(outputFormat, conversionContext(state))
	}
	if outputFormat == FormatChatCompletions {
		converter = &chatSSEConverter{converter: converter}
	}
	return newPublicStreamConverter(converter, upstreamFormat, outputFormat), nil
}

type validatedGeminiStream struct {
	converted StreamConverter
	validator StreamConverter
	failure   *Error
}

func (s *validatedGeminiStream) Feed(chunk []byte) ([][]byte, bool, *Error) {
	if s.failure != nil {
		return nil, false, s.failure
	}
	frames, _, validationErr := s.validator.Feed(chunk)
	out, done, err := s.converted.Feed(bytes.Join(frames, nil))
	if err == nil {
		err = validationErr
	}
	s.failure = err
	return out, done && err == nil, err
}

func (s *validatedGeminiStream) Finish() ([][]byte, *Error) {
	if s.failure != nil {
		return nil, s.failure
	}
	frames, validationErr := s.validator.Finish()
	out, _, err := s.converted.Feed(bytes.Join(frames, nil))
	if err == nil {
		err = validationErr
	}
	if err == nil {
		var tail [][]byte
		tail, err = s.converted.Finish()
		out = append(out, tail...)
	}
	s.failure = err
	return out, err
}

// Internal Chat Completions adapters return data payloads. Frame them here so
// callers can write all public stream outputs directly to an SSE connection.
type chatSSEConverter struct {
	converter StreamConverter
	done      bool
}

func (sc *chatSSEConverter) Feed(chunk []byte) ([][]byte, bool, *Error) {
	if sc.done {
		return nil, true, nil
	}
	events, done, err := sc.converter.Feed(chunk)
	return sc.frame(events, done && err == nil), done, err
}

func (sc *chatSSEConverter) Finish() ([][]byte, *Error) {
	if sc.done {
		return nil, nil
	}
	events, err := sc.converter.Finish()
	return sc.frame(events, err == nil), err
}

func (sc *chatSSEConverter) frame(payloads [][]byte, done bool) [][]byte {
	events := make([][]byte, 0, len(payloads)+1)
	for _, payload := range payloads {
		frame := append([]byte("data: "), bytes.ReplaceAll(payload, []byte("\n"), []byte("\ndata: "))...)
		events = append(events, append(frame, '\n', '\n'))
	}
	if done {
		events = append(events, []byte("data: [DONE]\n\n"))
		sc.done = true
	}
	return events
}

// The transport boundary parses Chat Completions as complete SSE frames;
// internal adapters receive one normalized data line per frame.
type publicStreamConverter struct {
	converter StreamConverter
	input     *shared.SSEFramer
	sequence  int64
	sequenced bool
	done      bool
	failure   *Error
}

func newPublicStreamConverter(converter StreamConverter, from, to string) *publicStreamConverter {
	sc := &publicStreamConverter{converter: converter, sequenced: to == FormatResponses && from != to}
	if from == FormatChatCompletions {
		sc.input = shared.NewSSEFramer(false)
	}
	return sc
}

func (sc *publicStreamConverter) Feed(chunk []byte) ([][]byte, bool, *Error) {
	if sc.failure != nil {
		return nil, false, sc.failure
	}
	if sc.done {
		return nil, true, nil
	}
	var events [][]byte
	var done bool
	var err *Error
	if sc.input == nil {
		events, done, err = sc.converter.Feed(chunk)
	} else {
		sc.input.Push(chunk)
		events, done, err = sc.drain()
	}
	if err != nil {
		sc.failure = err
	}
	sc.done = done && err == nil
	return sc.encode(events), done, err
}

func (sc *publicStreamConverter) drain() ([][]byte, bool, *Error) {
	var events [][]byte
	for {
		_, data, _, ok := sc.input.Next()
		if !ok {
			return events, false, nil
		}
		if data == "" {
			continue
		}
		if !shared.IsSSEDone(data) {
			var compact bytes.Buffer
			if err := json.Compact(&compact, []byte(data)); err != nil || !shared.IsJSONObject(data) {
				return events, false, Translation("malformed Chat Completions SSE payload: " + shared.RedactedSnippet(data))
			}
			data = compact.String()
		}
		out, done, err := sc.converter.Feed([]byte("data: " + data + "\n"))
		events = append(events, out...)
		if err != nil || done {
			return events, done, err
		}
	}
}

func (sc *publicStreamConverter) Finish() ([][]byte, *Error) {
	if sc.failure != nil {
		return nil, sc.failure
	}
	if sc.done {
		return nil, nil
	}
	var events [][]byte
	if sc.input != nil {
		sc.input.End()
		out, done, err := sc.drain()
		events = append(events, out...)
		if err != nil {
			sc.failure = err
			return sc.encode(events), err
		}
		if done {
			sc.done = true
			return sc.encode(events), nil
		}
	}
	tail, err := sc.converter.Finish()
	events = append(events, tail...)
	if err != nil {
		sc.failure = err
	} else {
		sc.done = true
	}
	return sc.encode(events), err
}

func (sc *publicStreamConverter) encode(events [][]byte) [][]byte {
	if !sc.sequenced {
		return events
	}
	for i, frame := range events {
		framer := shared.NewSSEFramer(false)
		framer.Push(frame)
		name, data, _, ok := framer.Next()
		if !ok {
			continue
		}
		var doc map[string]json.RawMessage
		if json.Unmarshal([]byte(data), &doc) != nil || doc == nil {
			continue
		}
		number, _ := json.Marshal(sc.sequence)
		sc.sequence++
		doc["sequence_number"] = number
		events[i] = shared.SSEEvent(name, doc)
	}
	return events
}
