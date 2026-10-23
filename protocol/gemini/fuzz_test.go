package gemini_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/dillonzq/apix"
	"github.com/dillonzq/apix/protocol/anthropic"
	"github.com/dillonzq/apix/protocol/gemini"
	"github.com/dillonzq/apix/protocol/openai"
	"github.com/dillonzq/apix/sse"
)

func FuzzGeminiStreamChunkBoundaries(f *testing.F) {
	ps := []apix.Protocol{gemini.GenerateContent, openai.ChatCompletions, openai.Responses, anthropic.Messages}
	for i, p := range ps {
		f.Add([]byte(fixtures[p].stream), uint8(i))
		f.Add([]byte("data: {broken\n\n"), uint8(i))
	}
	f.Fuzz(func(t *testing.T, wire []byte, index uint8) {
		if len(wire) > 1<<16 {
			t.Skip()
		}
		from := ps[int(index)%len(ps)]
		to := gemini.GenerateContent
		if from == gemini.GenerateContent {
			to = ps[int(index/4)%len(ps)]
		}
		r := registry(t)
		run := func(step int) ([]string, apix.Class) {
			ex, err := r.NewExchange(to, from, apix.ExchangeOptions{})
			if err != nil {
				t.Fatal(err)
			}
			sc, err := ex.NewStreamConverter()
			if err != nil {
				t.Fatal(err)
			}
			var frames [][]byte
			var failure error
			for i := 0; i < len(wire); i += step {
				events, done, e := sc.Feed(wire[i:min(i+step, len(wire))])
				frames = append(frames, events...)
				if e != nil {
					failure = e
					break
				}
				if done {
					break
				}
			}
			if failure == nil {
				tail, e := sc.Finish()
				frames = append(frames, tail...)
				failure = e
			}
			class := apix.Class("")
			if failure != nil {
				class = apix.AsError(failure).Class
				if ev, done, e := sc.Feed([]byte(fixtures[from].stream)); !errors.Is(e, failure) || done || len(ev) > 0 {
					t.Fatal("failure is not terminal")
				}
				if ev, e := sc.Finish(); !errors.Is(e, failure) || len(ev) > 0 {
					t.Fatal("Finish recovered from failure")
				}
			}
			var result []string
			var decoder sse.Decoder
			for _, frame := range frames {
				for _, event := range decoder.Feed(frame) {
					var value any
					if json.Unmarshal(event.Data, &value) == nil {
						clearTimes(value)
						data, _ := json.Marshal(value)
						result = append(result, event.Event+":"+string(data))
					} else {
						result = append(result, fmt.Sprint(event.Done)+string(event.Data))
					}
				}
			}
			return result, class
		}
		whole, class := run(max(len(wire), 1))
		chunked, chunkClass := run(1)
		if class != chunkClass || !reflect.DeepEqual(whole, chunked) {
			t.Fatalf("chunk-dependent results: %s %v / %s %v", class, whole, chunkClass, chunked)
		}
	})
}
func clearTimes(v any) {
	switch x := v.(type) {
	case map[string]any:
		for k, v := range x {
			if k == "created" || k == "created_at" {
				x[k] = 0
			} else {
				clearTimes(v)
			}
		}
	case []any:
		for _, v := range x {
			clearTimes(v)
		}
	}
}
