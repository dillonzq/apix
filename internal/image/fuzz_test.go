package image_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/dillonzq/apix"
	"github.com/dillonzq/apix/protocol/gemini"
	"github.com/dillonzq/apix/protocol/openai"
	"github.com/dillonzq/apix/sse"
)

// streamCase is a native stream: the request that sets it up and a valid
// upstream stream.
type streamCase struct {
	protocol apix.Protocol
	request  apix.Payload
	wire     string
	// terminal reports whether a decoded event lets the stream end.
	terminal func(event, data string) bool
}

var streamCases = []streamCase{
	{openai.ImagesGenerations, apix.Payload{Body: []byte(`{"prompt":"p","stream":true,"partial_images":1}`)},
		"event: image_generation.partial_image\ndata: {\"type\":\"image_generation.partial_image\",\"b64_json\":\"cA==\",\"partial_image_index\":0}\n\nevent: image_generation.completed\ndata: {\"type\":\"image_generation.completed\",\"b64_json\":\"aW1n\",\"usage\":{\"input_tokens\":1,\"output_tokens\":2,\"total_tokens\":3}}\n\n",
		completedEvent("image_generation")},
	{openai.ImagesGenerations, apix.Payload{Body: []byte(`{"prompt":"p","stream":true,"n":2}`)},
		"data: {\"type\":\"image_generation.completed\",\"b64_json\":\"YQ==\"}\r\n\r\n: ping\r\n\r\ndata: {\"type\":\"image_generation.completed\",\"b64_json\":\"Yg==\"}\r\n\r\n",
		completedEvent("image_generation")},
	{openai.ImagesEdits, apix.Payload{Body: []byte(`{"prompt":"p","stream":true,"images":[{"image_url":"data:image/png;base64,aW1n"}]}`)},
		"event: image_edit.completed\ndata: {\"type\":\"image_edit.completed\",\"b64_json\":\"aW1n\"}",
		completedEvent("image_edit")},
	{gemini.GenerateContentImage, apix.Payload{Body: []byte(`{"contents":[{"parts":[{"text":"p"}]}]}`)},
		"data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"ok\"}]}}]}\n\ndata: {\"candidates\":[{\"content\":{\"parts\":[{\"inlineData\":{\"mimeType\":\"image/png\",\"data\":\"aW1n\"}}]},\"finishReason\":\"STOP\"}]}\n\n",
		// A loose check: Gemini accepts camelCase and snake_case names.
		func(_, data string) bool {
			return strings.Contains(data, "inishReason") || strings.Contains(data, "inish_reason") ||
				strings.Contains(data, "lockReason") || strings.Contains(data, "lock_reason")
		}},
}

// completedEvent recognizes an OpenAI completed event by its type, or by
// its SSE event name when the data has no type.
func completedEvent(prefix string) func(event, data string) bool {
	return func(event, data string) bool {
		var ev map[string]json.RawMessage
		json.Unmarshal([]byte(data), &ev)
		var typ string
		json.Unmarshal(ev["type"], &typ)
		if typ == "" {
			typ = event
		}
		return typ == prefix+".completed"
	}
}

// FuzzImageStreamChunkBoundaries checks native image streams: results do not
// depend on chunk boundaries, every emitted frame is one valid JSON event, a
// successful stream carries a terminal event, and failures are terminal.
func FuzzImageStreamChunkBoundaries(f *testing.F) {
	for i, c := range streamCases {
		f.Add([]byte(c.wire), uint8(i))
		f.Add([]byte(c.wire[:len(c.wire)/2]), uint8(i))
		f.Add([]byte("data: {broken\n\n"), uint8(i))
		f.Add([]byte("event: error\ndata: {\"type\":\"error\",\"error\":{\"message\":\"m\"}}\n\n"), uint8(i))
	}
	f.Fuzz(func(t *testing.T, wire []byte, index uint8) {
		if len(wire) > 1<<16 {
			t.Skip()
		}
		c := streamCases[int(index)%len(streamCases)]
		stream := true
		request := c.request
		request.Metadata = apix.RequestMetadata{Stream: &stream}
		run := func(step int) ([]string, bool, apix.Class) {
			ex := exchange(t, c.protocol, c.protocol, apix.ExchangeOptions{})
			if _, err := ex.ConvertRequest(request, "m"); err != nil {
				t.Fatal(err)
			}
			sc, err := ex.NewStreamConverter()
			if err != nil {
				t.Fatal(err)
			}
			var frames [][]byte
			var failure error
			done := false
			for i := 0; i < len(wire) && !done; i += step {
				var events [][]byte
				events, done, failure = sc.Feed(wire[i:min(i+step, len(wire))])
				frames = append(frames, events...)
				if failure != nil {
					break
				}
			}
			if failure == nil && !done {
				tail, err := sc.Finish()
				frames = append(frames, tail...)
				failure = err
			}
			var class apix.Class
			if failure != nil {
				class = apix.AsError(failure).Class
				if events, done, err := sc.Feed([]byte(c.wire)); !errors.Is(err, failure) || done || len(events) > 0 {
					t.Fatal("Feed recovered from failure")
				}
				if events, err := sc.Finish(); !errors.Is(err, failure) || len(events) > 0 {
					t.Fatal("Finish recovered from failure")
				}
			}
			var result []string
			terminal := false
			for _, frame := range frames {
				var decoder sse.Decoder
				events := append(decoder.Feed(frame), decoder.Finish()...)
				if len(events) != 1 || !json.Valid(events[0].Data) {
					t.Fatalf("emitted frame is not one JSON event: %q", frame)
				}
				terminal = terminal || c.terminal(events[0].Event, string(events[0].Data))
				result = append(result, events[0].Event+":"+string(events[0].Data))
			}
			if failure == nil && !terminal {
				t.Fatalf("stream succeeded without a terminal event: %q", wire)
			}
			return result, done, class
		}
		whole, wholeDone, wholeClass := run(max(len(wire), 1))
		for _, step := range []int{1, 7} {
			got, done, class := run(step)
			if class != wholeClass || done != wholeDone || !reflect.DeepEqual(got, whole) {
				t.Fatalf("step %d: %s %v %v, whole: %s %v %v", step, class, done, got, wholeClass, wholeDone, whole)
			}
		}
	})
}

// directions lists every image client/upstream pair; the output is the client.
func directions() [][2]apix.Protocol {
	reg, err := apix.NewRegistry(slices.Concat(openai.Codecs(), gemini.Codecs())...)
	if err != nil {
		panic(err)
	}
	var out [][2]apix.Protocol
	for _, client := range reg.Protocols() {
		for _, upstream := range reg.Protocols() {
			if k, _ := reg.Kind(client); k == apix.KindImage && reg.CanConvert(client, upstream) {
				out = append(out, [2]apix.Protocol{client, upstream})
			}
		}
	}
	return out
}

var fuzzRequests = map[apix.Protocol]apix.Payload{
	openai.ImagesGenerations:    {Body: []byte(`{"model":"m","prompt":"a cat","n":2,"size":"1536x1024","quality":"high"}`)},
	openai.ImagesEdits:          {Body: []byte(`{"prompt":"a hat","images":[{"image_url":"data:image/png;base64,aW1n"}],"size":"1024x1536"}`)},
	gemini.GenerateContentImage: {Body: []byte(`{"contents":[{"role":"user","parts":[{"text":"a hat"},{"inlineData":{"mimeType":"image/png","data":"aW1n"}}]}],"generationConfig":{"candidateCount":2,"imageConfig":{"aspectRatio":"16:9","imageSize":"2K"}}}`)},
	gemini.Imagen:               {Body: []byte(`{"instances":[{"prompt":"a fox"}],"parameters":{"sampleCount":2,"aspectRatio":"3:4","outputOptions":{"mimeType":"image/jpeg"}}}`)},
}

// FuzzImageRequest converts hostile requests in every direction. Conversion
// never panics, fails only with an *apix.Error, and a converted request is
// one the upstream protocol itself accepts.
func FuzzImageRequest(f *testing.F) {
	for i, d := range directions() {
		f.Add(fuzzRequests[d[0]].Body, uint8(i), false)
		f.Add(fuzzRequests[d[0]].Body, uint8(i), true)
	}
	f.Add([]byte(`{"contents":[{"parts":[{"text":"p"},{"inlineData":{"mimeType":"image/png\r\nX-Injected: 1","data":"aW1n"}}]}]}`), uint8(0), false)
	f.Fuzz(func(t *testing.T, body []byte, index uint8, multipart bool) {
		if len(body) > 1<<16 {
			t.Skip()
		}
		reg := registry(t)
		dirs := directions()
		d := dirs[int(index)%len(dirs)]
		req := apix.Payload{Body: body}
		if multipart && d[0] == openai.ImagesEdits {
			req = form(t, map[string]string{"prompt": string(body), "size": string(body)}, [2]string{"image", string(body)})
		}
		if _, err := reg.Inspect(d[0], req); err != nil {
			if apix.AsError(err).Class == "" {
				t.Fatalf("unclassified error %v", err)
			}
			return
		}
		ex, err := reg.NewExchange(d[0], d[1], apix.ExchangeOptions{})
		if err != nil {
			t.Fatal(err)
		}
		out, err := ex.ConvertRequest(req, "up")
		if err != nil {
			if apix.AsError(err).Class == "" {
				t.Fatalf("unclassified error %v", err)
			}
			return
		}
		if _, err := reg.Inspect(d[1], out); err != nil {
			t.Fatalf("%s → %s produced a request %s rejects: %v\n%s", d[0], d[1], d[1], err, out.Body)
		}
		// Native forms keep the client's own parts; converted forms are built
		// from parsed values only.
		if d[0] != d[1] && strings.HasPrefix(out.ContentType, "multipart/") {
			checkFormHeaders(t, out)
		}
	})
}

var fuzzResponses = map[apix.Protocol]string{
	openai.ImagesGenerations:    `{"created":1,"data":[{"b64_json":"aW1n","revised_prompt":"r"}],"output_format":"webp","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3,"input_tokens_details":{"text_tokens":1}}}`,
	openai.ImagesEdits:          `{"created":1,"data":[{"b64_json":"aW1n"},{"url":"https://example.com/a.png"}]}`,
	gemini.GenerateContentImage: `{"candidates":[{"content":{"parts":[{"text":"t"},{"inline_data":{"mime_type":"image/png","data":"aW1n"},"thought":true},{"inlineData":{"mimeType":"image/jpeg","data":"aW1n"}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokensDetails":[{"modality":"IMAGE","tokenCount":2}]}}`,
	gemini.Imagen:               `{"predictions":[{"raiFilteredReason":"x"},{"bytesBase64Encoded":"aW1n","mimeType":"image/png"}]}`,
}

// FuzzImageResponse converts hostile upstream responses in every direction.
// Conversion never panics, fails only with an *apix.Error, and a converted
// response is one the output protocol's own exchange accepts.
func FuzzImageResponse(f *testing.F) {
	for i, d := range directions() {
		f.Add([]byte(fuzzResponses[d[1]]), uint8(i), false)
		f.Add([]byte(fuzzResponses[d[1]]), uint8(i), true)
	}
	f.Add([]byte(`{"candidates":[{"content":{"parts":null}}],"promptFeedback":{"blockReason":"SAFETY"}}`), uint8(3), true)
	f.Fuzz(func(t *testing.T, body []byte, index uint8, stream bool) {
		if len(body) > 1<<16 {
			t.Skip()
		}
		dirs := directions()
		d := dirs[int(index)%len(dirs)]
		client, upstream := d[0], d[1]
		ex := exchange(t, client, upstream, apix.ExchangeOptions{})
		req := fuzzRequests[client]
		if stream {
			on := true
			req.Metadata.Stream = &on
		}
		if _, err := ex.ConvertRequest(req, "up"); err != nil {
			return // e.g. generations requests cannot reach edits
		}
		out, err := ex.ConvertResponse(200, apix.Payload{Body: body})
		if err != nil {
			if apix.AsError(err).Class == "" {
				t.Fatalf("unclassified error %v", err)
			}
			return
		}
		if out.ContentType == "text/event-stream" {
			var decoder sse.Decoder
			events := append(decoder.Feed(out.Body), decoder.Finish()...)
			if len(events) == 0 {
				t.Fatalf("empty emulated stream for %s", body)
			}
			for _, ev := range events {
				if !json.Valid(ev.Data) {
					t.Fatalf("emulated event is not JSON: %q", ev.Data)
				}
			}
			return
		}
		if client == upstream {
			if !bytes.Equal(out.Body, body) {
				t.Fatal("native response changed")
			}
			return
		}
		native := exchange(t, client, client, apix.ExchangeOptions{})
		if _, err := native.ConvertRequest(fuzzRequests[client], "up"); err != nil {
			t.Fatal(err)
		}
		if _, err := native.ConvertResponse(200, out); err != nil {
			t.Fatalf("%s → %s produced a response %s rejects: %v\n%s", upstream, client, client, err, out.Body)
		}
	})
}

// checkFormHeaders requires every part of a converted form to carry only
// the headers conversion writes, so client values cannot inject headers.
func checkFormHeaders(t *testing.T, p apix.Payload) {
	t.Helper()
	_, params, err := mime.ParseMediaType(p.ContentType)
	if err != nil {
		t.Fatal(err)
	}
	r := multipart.NewReader(bytes.NewReader(p.Body), params["boundary"])
	for {
		part, err := r.NextPart()
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			t.Fatalf("converted form does not parse: %v", err)
		}
		for k := range part.Header {
			if k != "Content-Disposition" && k != "Content-Type" {
				t.Fatalf("unexpected part header %q", k)
			}
		}
		if part.FileName() != "" {
			mt, ps, err := mime.ParseMediaType(part.Header.Get("Content-Type"))
			if err != nil || !strings.HasPrefix(mt, "image/") || len(ps) > 0 {
				t.Fatalf("file part content type %q", part.Header.Get("Content-Type"))
			}
		}
	}
}
