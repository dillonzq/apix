package sse

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

// refFramer is Framer as it was before Next resumed its line search: every
// call searched the whole buffer. It is the reference the fuzz target holds
// the incremental search to.
type refFramer struct {
	buf       []byte
	eventType string
	data      strings.Builder
	dataLines int
	sawFrame  bool
	raw       []byte
}

func (f *refFramer) next() (eventType, data string, raw []byte, ok bool) {
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
		f.raw = append(f.raw, f.buf[:end]...)
		text := string(line)
		f.buf = f.buf[end:]
		switch {
		case text == "":
			if !f.sawFrame {
				f.raw = nil
				continue
			}
			eventType, data, raw = f.eventType, f.data.String(), f.raw
			f.eventType = ""
			f.data.Reset()
			f.dataLines = 0
			f.sawFrame = false
			f.raw = nil
			return eventType, data, raw, true
		case strings.HasPrefix(text, ":"):
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
		}
	}
}

type frame struct {
	Event, Data, Raw string
}

// Next must frame exactly as the full-buffer search did, however the input
// is split, including a '\r' that ends one push and a '\n' that starts the
// next.
func FuzzFramerMatchesReference(f *testing.F) {
	for _, s := range []string{
		"event: a\ndata: {}\n\n",
		": hi\r\nevent: b\r\ndata: 1\r\ndata: 2\r\n\r\n",
		"data: x\r\rdata: y\r\n\r\n",
		"\n\ndata:\n\nevent:\n\n",
		"data: " + strings.Repeat("A", 300) + "\n\n",
	} {
		f.Add(s, uint8(1))
		f.Add(s, uint8(7))
	}
	f.Fuzz(func(t *testing.T, wire string, step uint8) {
		if len(wire) > 4096 {
			t.Skip()
		}
		n := int(step%32) + 1
		var got, want []frame
		fr := NewFramer(true)
		ref := &refFramer{}
		for i := 0; i <= len(wire); i += n {
			chunk := []byte(wire[i:min(i+n, len(wire))])
			if i+n > len(wire) {
				chunk = append(chunk, "\n\n"...) // End
			}
			fr.Push(chunk)
			ref.buf = append(ref.buf, chunk...)
			for {
				e, d, r, ok := fr.Next()
				if !ok {
					break
				}
				got = append(got, frame{e, d, string(r)})
			}
			for {
				e, d, r, ok := ref.next()
				if !ok {
					break
				}
				want = append(want, frame{e, d, string(r)})
			}
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("framer %q\nreference %q", got, want)
		}
	})
}

// BenchmarkFramerLongLine frames a 3 MiB data line, such as a partial image,
// pushed in 16 KiB network chunks.
func BenchmarkFramerLongLine(b *testing.B) {
	wire := []byte("event: image_generation.partial_image\ndata: {\"b64_json\":\"" + strings.Repeat("QUJD", 3<<20/4) + "\"}\n\n")
	const chunk = 16 << 10
	b.SetBytes(int64(len(wire)))
	b.ReportAllocs()
	for b.Loop() {
		fr := NewFramer(true)
		for i := 0; i < len(wire); i += chunk {
			fr.Push(wire[i:min(i+chunk, len(wire))])
			for {
				if _, _, _, ok := fr.Next(); !ok {
					break
				}
			}
		}
	}
}
