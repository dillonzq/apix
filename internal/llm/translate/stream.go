package translate

import (
	"bytes"
	"encoding/json"
	"strconv"

	"github.com/dillonzq/apix/internal/jsonedit"
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
	maximum := 0
	if state != nil {
		maximum = state.Options.MaxStreamFrameBytes
	}
	if upstreamFormat == FormatGemini || outputFormat == FormatGemini {
		if upstreamFormat == FormatGemini {
			return gemini.NewStreamConverter(upstreamFormat, outputFormat, conversionContext(state)), nil
		}
		converted := gemini.NewStreamConverter(upstreamFormat, outputFormat, conversionContext(state), -1)
		// Reuse source validation and complete source frames. Conversion remains
		// direct; validation must run before a target terminal can be emitted.
		validationState := NewRequestState()
		validationState.Options.MaxStreamFrameBytes = maximum
		validator, err := NewStreamConverter(upstreamFormat, upstreamFormat, validationState)
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
	return newPublicStreamConverter(converter, upstreamFormat, outputFormat, maximum), nil
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
	line      bytes.Buffer // the normalized data line drain feeds
	done      bool
	failure   *Error
}

func newPublicStreamConverter(converter StreamConverter, from, to string, maxFrameBytes ...int) *publicStreamConverter {
	sc := &publicStreamConverter{converter: converter, sequenced: to == FormatResponses && from != to}
	if from == FormatChatCompletions {
		sc.input = shared.NewSSEFramer(false, maxFrameBytes...)
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
			return events, false, shared.SSEFrameError(sc.input)
		}
		if data == "" {
			continue
		}
		// The converter copies what it keeps, so one line buffer serves
		// every event.
		sc.line.Reset()
		sc.line.WriteString("data: ")
		if shared.IsSSEDone(data) {
			sc.line.WriteString(data)
		} else if err := json.Compact(&sc.line, []byte(data)); err != nil || sc.line.Bytes()[len("data: ")] != '{' {
			// Compact validates the payload and elides the whitespace
			// around it, so its first byte tells an object apart.
			return events, false, Translation("malformed Chat Completions SSE payload: " + shared.RedactedSnippet(data))
		}
		sc.line.WriteByte('\n')
		out, done, err := sc.converter.Feed(sc.line.Bytes())
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
	var number []byte
	for i, frame := range events {
		name, data, ok := splitFrame(frame)
		if !ok {
			framer := shared.NewSSEFramer(false, -1)
			framer.Push(frame)
			var payload string
			if name, payload, _, ok = framer.Next(); !ok {
				continue
			}
			data = []byte(payload)
		}
		number = strconv.AppendInt(number[:0], sc.sequence, 10)
		if out, ok := jsonedit.Apply(data, jsonedit.Set(number, "sequence_number")); ok {
			sc.sequence++
			events[i] = shared.SSEFrame(name, out)
			continue
		}
		var doc map[string]json.RawMessage
		if json.Unmarshal(data, &doc) != nil || doc == nil {
			continue
		}
		sc.sequence++
		doc["sequence_number"] = number
		events[i] = shared.SSEEvent(name, doc)
	}
	return events
}

// splitFrame reads the event name and data of a frame in the single-line
// form shared.SSEFrame renders, "event: NAME\ndata: {...}\n\n", without a
// framer. It reports ok=false for any other frame, which the caller frames
// instead; where it succeeds, the framer would read the same name and data.
// data aliases frame.
func splitFrame(frame []byte) (name string, data []byte, ok bool) {
	rest, ok := bytes.CutPrefix(frame, []byte("event: "))
	if !ok {
		return "", nil, false
	}
	n := bytes.IndexByte(rest, '\n')
	if n <= 0 {
		return "", nil, false
	}
	// The framer trims the name; a printable ASCII name has nothing to trim.
	for _, c := range rest[:n] {
		if c <= ' ' || c > '~' {
			return "", nil, false
		}
	}
	data, ok = bytes.CutPrefix(rest[n+1:], []byte("data: "))
	if !ok {
		return "", nil, false
	}
	data, ok = bytes.CutSuffix(data, []byte("\n\n"))
	if !ok || len(data) == 0 || data[0] != '{' || bytes.ContainsAny(data, "\r\n") {
		return "", nil, false
	}
	return string(rest[:n]), data, true
}
