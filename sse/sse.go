// Package sse frames and unframes Server-Sent Events without interpreting
// their payloads. Converters and the gateway use it for every SSE protocol.
package sse

// Event is an unframed SSE event. Data joins multiple data lines with LF.
// Done identifies the Chat Completions [DONE] marker, not protocol completion
// for Responses or Messages. Event is the optional event field.
type Event struct {
	Event string
	Data  []byte
	Done  bool
}

// Decoder removes SSE framing without interpreting JSON. Its zero value is
// ready to use. Use one decoder per stream, sequentially; call Finish at EOF.
// This lets hosts that add their own data: framing consume converter output.
type Decoder struct {
	framer *Framer
	closed bool
}

func (d *Decoder) drain() []Event {
	var events []Event
	for {
		name, data, _, ok := d.framer.Next()
		if !ok {
			return events
		}
		events = append(events, Event{Event: name, Data: []byte(data), Done: IsDone(data)})
	}
}

// Feed accepts arbitrary chunks, including complete StreamConverter frames.
// Comments are ignored. Calls after Finish return no events.
func (d *Decoder) Feed(chunk []byte) []Event {
	if d.closed {
		return nil
	}
	if d.framer == nil {
		d.framer = NewFramer(false)
	}
	d.framer.Push(chunk)
	return d.drain()
}

// Finish flushes an event missing its final line separator and closes decoding.
// It does not validate protocol completion; StreamConverter.Finish does that.
func (d *Decoder) Finish() []Event {
	if d.closed {
		return nil
	}
	d.closed = true
	if d.framer == nil {
		return nil
	}
	d.framer.End()
	return d.drain()
}
