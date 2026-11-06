package codec

import (
	"encoding/json"

	"github.com/dillonzq/apix"
	"github.com/dillonzq/apix/internal/jsonedit"
	"github.com/dillonzq/apix/internal/llm/adapter/shared"
	"github.com/dillonzq/apix/internal/usage"
)

// recorder captures the ID of state an upstream stores, such as a Responses
// response, so the exchange can report it through apix.StateReporter. A nil
// recorder records nothing.
type recorder struct {
	kind string // affinity key prefix and the event object holding the ID
	id   string
}

func (r *recorder) keys() []string {
	if r == nil || r.id == "" {
		return nil
	}
	return []string{r.kind + ":" + r.id}
}

// body records the top-level id of an ordinary response.
func (r *recorder) body(b []byte) {
	if r == nil || r.id != "" {
		return
	}
	if f, ok := jsonedit.Fields(b, "id"); ok {
		// A missing, null or non-string id records nothing, as below.
		r.id, _ = jsonedit.String(f[0])
		return
	}
	var doc struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(b, &doc) == nil {
		r.id = doc.ID
	}
}

// events records the nested id of the first stream event that carries one,
// such as response.created or interaction.created.
func (r *recorder) events(events [][]byte) {
	if r == nil || r.id != "" {
		return
	}
	for _, event := range events {
		framer := shared.NewSSEFramer(false)
		framer.Push(event)
		framer.End()
		for {
			_, data, _, ok := framer.Next()
			if !ok {
				break
			}
			var doc map[string]json.RawMessage
			if json.Unmarshal([]byte(data), &doc) != nil {
				continue
			}
			var nested struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(doc[r.kind], &nested) == nil && nested.ID != "" {
				r.id = nested.ID
				return
			}
		}
	}
}

// engineStream is the engine's stream converter, which returns concrete
// errors.
type engineStream interface {
	Feed(chunk []byte) ([][]byte, bool, *apix.Error)
	Finish() ([][]byte, *apix.Error)
}

// stream adapts an engine stream to apix.StreamConverter, records created
// state from its output, and tracks usage from its upstream input.
type stream struct {
	engine   engineStream
	recorder *recorder
	usage    *usage.Tracker
}

func (s *stream) Feed(chunk []byte) ([][]byte, bool, error) {
	s.usage.Feed(chunk)
	events, done, err := s.engine.Feed(chunk)
	s.recorder.events(events)
	if err != nil {
		return events, done, err
	}
	return events, done, nil
}

func (s *stream) Finish() ([][]byte, error) {
	s.usage.End()
	events, err := s.engine.Finish()
	s.recorder.events(events)
	if err != nil {
		return events, err
	}
	return events, nil
}
