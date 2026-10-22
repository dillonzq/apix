package sse

import (
	"bytes"
	"strings"
)

// Framer buffers SSE bytes across chunks and yields complete frames.
// Multiple data: lines within one frame join with "\n" per the SSE spec;
// comments, heartbeats, and stray blank lines are skipped; a frame is
// emitted only when at least one event:/data: line was seen. Raw blocks
// are captured verbatim (including line terminators) only when wantRaw is
// set — pass false when no consumer reads them.
type Framer struct {
	buf       []byte          // partial line carried across Push calls
	eventType string          // current event: value
	data      strings.Builder // joined data: payloads of the current frame
	dataLines int
	sawFrame  bool // event:/data: seen since last emit
	wantRaw   bool
	raw       []byte // verbatim current block when wantRaw
}

// NewFramer prepares a framer; wantRaw enables verbatim block capture.
func NewFramer(wantRaw bool) *Framer {
	return &Framer{wantRaw: wantRaw}
}

// Push appends one network chunk to the framer buffer.
func (f *Framer) Push(chunk []byte) {
	f.buf = append(f.buf, chunk...)
}

// End frames the final data line at EOF, including a valid SSE event whose
// final newline/blank separator was omitted. Incomplete JSON still fails in
// the converter; residual bytes must not disappear behind a success terminal.
func (f *Framer) End() { f.Push([]byte("\n\n")) }

// Next returns the next complete frame (eventType, joined data, raw block).
// ok=false means the buffer needs more input.
func (f *Framer) Next() (eventType, data string, raw []byte, ok bool) {
	for {
		i := bytes.IndexAny(f.buf, "\r\n")
		if i < 0 {
			return "", "", nil, false
		}
		if f.buf[i] == '\r' && i+1 == len(f.buf) {
			return "", "", nil, false
		}
		end := i + 1
		if f.buf[i] == '\r' && end < len(f.buf) && f.buf[end] == '\n' {
			end++
		}
		line := f.buf[:i]
		if f.wantRaw {
			f.raw = append(f.raw, f.buf[:end]...)
		}
		text := string(line)
		f.buf = f.buf[end:]
		switch {
		case text == "":
			if !f.sawFrame {
				f.raw = nil
				continue // stray blank line between frames
			}
			eventType, data, raw = f.eventType, f.data.String(), f.raw
			f.eventType = ""
			f.data.Reset()
			f.dataLines = 0
			f.sawFrame = false
			f.raw = nil
			return eventType, data, raw, true
		case strings.HasPrefix(text, ":"):
			// SSE comment/heartbeat: raw capture only.
		case strings.HasPrefix(text, "event:"):
			f.eventType = strings.TrimSpace(strings.TrimPrefix(text, "event:"))
			f.sawFrame = true
		case strings.HasPrefix(text, "data:"):
			if f.dataLines > 0 {
				f.data.WriteByte('\n')
			}
			f.dataLines++
			f.data.WriteString(strings.TrimPrefix(strings.TrimPrefix(text, "data:"), " "))
			f.sawFrame = true
		default:
			// Unknown lines are ignored outside raw capture.
		}
	}
}

// IsDone reports whether a framer-stripped SSE data payload is the
// terminal [DONE] sentinel; surrounding whitespace and CR are ignored.
func IsDone(payload string) bool {
	return strings.TrimSpace(payload) == "[DONE]"
}
