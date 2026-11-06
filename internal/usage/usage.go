// Package usage reads the token usage an upstream reports, from an ordinary
// response body or from its raw stream bytes, and normalizes it to
// apix.Usage. It reads upstream bytes rather than converted output, so the
// counts are the upstream's own, including on same-protocol paths.
package usage

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/dillonzq/apix"
	"github.com/dillonzq/apix/internal/jsonedit"
	"github.com/dillonzq/apix/sse"
)

// Dialect names the usage fields of one family of protocols.
type Dialect int

const (
	// ChatCompletions reads prompt_tokens and completion_tokens.
	ChatCompletions Dialect = iota + 1
	// Responses reads input_tokens and output_tokens with OpenAI details;
	// OpenAI Images use the same fields.
	Responses
	// Messages reads input_tokens, output_tokens and the separate cache
	// counts, which it adds to the input.
	Messages
	// Gemini reads usageMetadata.
	Gemini
	// Interactions reads total_input_tokens and total_output_tokens.
	Interactions
)

// paths lists where a usage object may sit in a body or a stream event. The
// first that exists wins.
var paths = map[Dialect][][]string{
	ChatCompletions: {{"usage"}},
	Responses:       {{"usage"}, {"response", "usage"}},
	Messages:        {{"usage"}, {"message", "usage"}},
	Gemini:          {{"usageMetadata"}, {"usage_metadata"}},
	Interactions:    {{"usage"}, {"interaction", "usage"}},
}

// Tracker accumulates the usage of one execution. A nil Tracker tracks
// nothing. It is not safe for concurrent use.
type Tracker struct {
	dialect Dialect
	framer  *sse.Framer
	merged  map[string]json.RawMessage
	keys    []string // member order of merged, first appearance first
	single  json.RawMessage
	objects int
}

// New returns a tracker for d, or nil for the zero Dialect: a protocol that
// reports no usage.
func New(d Dialect) *Tracker {
	if d == 0 {
		return nil
	}
	return &Tracker{dialect: d}
}

// Body records the usage of an ordinary response.
func (t *Tracker) Body(body []byte) {
	if t == nil {
		return
	}
	t.record(body)
}

// Feed records usage from a chunk of an upstream SSE stream.
func (t *Tracker) Feed(chunk []byte) {
	if t == nil {
		return
	}
	if t.framer == nil {
		t.framer = sse.NewFramer(false)
	}
	t.framer.Push(chunk)
	t.drain()
}

// End frames a final event whose separator the stream omitted.
func (t *Tracker) End() {
	if t == nil || t.framer == nil {
		return
	}
	t.framer.End()
	t.drain()
}

func (t *Tracker) drain() {
	for {
		_, data, _, ok := t.framer.Next()
		if !ok {
			return
		}
		// Every usage key contains "sage"; skip the JSON walk for events
		// that cannot hold one, such as text deltas.
		if !strings.Contains(data, "sage") {
			continue
		}
		t.record([]byte(data))
	}
}

// record merges the usage object found in doc, if any.
func (t *Tracker) record(doc []byte) {
	if !bytes.Contains(doc, []byte("sage")) {
		return
	}
	raw := find(doc, paths[t.dialect])
	if raw == nil {
		return
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil || obj == nil {
		return
	}
	if t.merged == nil {
		t.merged = make(map[string]json.RawMessage, len(obj))
	}
	for _, k := range orderedKeys(raw, obj) {
		if _, seen := t.merged[k]; !seen {
			t.keys = append(t.keys, k)
		}
		t.merged[k] = obj[k]
	}
	t.objects++
	t.single = raw
}

// Usage returns the normalized usage recorded so far.
func (t *Tracker) Usage() (apix.Usage, bool) {
	if t == nil || t.objects == 0 {
		return apix.Usage{}, false
	}
	u := normalize(t.dialect, t.merged)
	if t.objects == 1 {
		u.Raw = append(json.RawMessage(nil), t.single...)
	} else {
		u.Raw = t.encodeMerged()
	}
	return u, true
}

// encodeMerged renders merged members in first-appearance order.
func (t *Tracker) encodeMerged() json.RawMessage {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range t.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		name, _ := json.Marshal(k)
		b.Write(name)
		b.WriteByte(':')
		b.Write(t.merged[k])
	}
	b.WriteByte('}')
	return b.Bytes()
}

// find returns a copy of the first object at one of paths in doc, or nil.
// The fast path reads members the way encoding/json would; ambiguous keys
// fall back to encoding/json.
func find(doc []byte, paths [][]string) json.RawMessage {
	root, ok := jsonedit.Object(doc)
	if !ok {
		return findSlow(doc, paths)
	}
	for _, path := range paths {
		cur := root
		found := true
		for _, key := range path {
			if !cur.IsObject() {
				found = false
				break
			}
			m, ok := jsonedit.Members(cur, key)
			if !ok {
				return findSlow(doc, paths)
			}
			cur = m[0]
			if !cur.Exists() {
				found = false
				break
			}
		}
		if found && cur.IsObject() {
			return append(json.RawMessage(nil), jsonedit.Raw(cur)...)
		}
	}
	return nil
}

func findSlow(doc []byte, paths [][]string) json.RawMessage {
	for _, path := range paths {
		cur := json.RawMessage(doc)
		for _, key := range path {
			var obj map[string]json.RawMessage
			if json.Unmarshal(cur, &obj) != nil || obj == nil {
				cur = nil
				break
			}
			cur = obj[key]
		}
		if cur == nil {
			continue
		}
		var obj map[string]json.RawMessage
		if json.Unmarshal(cur, &obj) == nil && obj != nil {
			return append(json.RawMessage(nil), cur...)
		}
	}
	return nil
}

// orderedKeys returns obj's keys in the order raw lists them.
func orderedKeys(raw []byte, obj map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(obj))
	seen := make(map[string]bool, len(obj))
	dec := json.NewDecoder(bytes.NewReader(raw))
	if _, err := dec.Token(); err == nil {
		for dec.More() {
			tok, err := dec.Token()
			if err != nil {
				break
			}
			k, _ := tok.(string)
			var skip json.RawMessage
			if dec.Decode(&skip) != nil {
				break
			}
			if _, ok := obj[k]; ok && !seen[k] {
				seen[k] = true
				keys = append(keys, k)
			}
		}
	}
	// Anything the walk missed keeps a deterministic place at the end.
	for k := range obj {
		if !seen[k] {
			keys = append(keys, k)
		}
	}
	return keys
}

func normalize(d Dialect, m map[string]json.RawMessage) apix.Usage {
	var u apix.Usage
	total := num(m, "total_tokens")
	switch d {
	case ChatCompletions:
		u.InputTokens = num(m, "prompt_tokens")
		u.OutputTokens = num(m, "completion_tokens")
		u.CacheReadTokens = num(m, "prompt_tokens_details", "cached_tokens")
		u.CacheWriteTokens = num(m, "prompt_tokens_details", "cache_write_tokens")
		u.ReasoningTokens = num(m, "completion_tokens_details", "reasoning_tokens")
	case Responses:
		u.InputTokens = num(m, "input_tokens")
		u.OutputTokens = num(m, "output_tokens")
		u.CacheReadTokens = num(m, "input_tokens_details", "cached_tokens")
		u.CacheWriteTokens = num(m, "input_tokens_details", "cache_write_tokens")
		u.ReasoningTokens = num(m, "output_tokens_details", "reasoning_tokens")
	case Messages:
		u.CacheReadTokens = num(m, "cache_read_input_tokens")
		u.CacheWriteTokens = num(m, "cache_creation_input_tokens")
		u.InputTokens = num(m, "input_tokens") + u.CacheReadTokens + u.CacheWriteTokens
		u.OutputTokens = num(m, "output_tokens")
		total = 0 // Messages reports no total
	case Gemini:
		u.CacheReadTokens = first(m, "cachedContentTokenCount", "cached_content_token_count")
		u.ReasoningTokens = first(m, "thoughtsTokenCount", "thoughts_token_count")
		u.InputTokens = first(m, "promptTokenCount", "prompt_token_count") + first(m, "toolUsePromptTokenCount", "tool_use_prompt_token_count")
		u.OutputTokens = first(m, "candidatesTokenCount", "candidates_token_count") + u.ReasoningTokens
		total = first(m, "totalTokenCount", "total_token_count")
	case Interactions:
		u.CacheReadTokens = num(m, "total_cached_tokens")
		u.ReasoningTokens = first(m, "total_thought_tokens", "total_reasoning_tokens")
		u.InputTokens = num(m, "total_input_tokens") + num(m, "total_tool_use_tokens")
		u.OutputTokens = num(m, "total_output_tokens") + u.ReasoningTokens
	}
	u.TotalTokens = total
	if u.TotalTokens == 0 {
		u.TotalTokens = u.InputTokens + u.OutputTokens
	}
	return u
}

// num reads a non-negative integer at path; anything else reads as zero.
func num(m map[string]json.RawMessage, path ...string) int64 {
	raw := m[path[0]]
	for _, key := range path[1:] {
		var obj map[string]json.RawMessage
		if json.Unmarshal(raw, &obj) != nil {
			return 0
		}
		raw = obj[key]
	}
	var n int64
	if json.Unmarshal(raw, &n) != nil || n < 0 {
		return 0
	}
	return n
}

// first reads the first of keys that is present.
func first(m map[string]json.RawMessage, keys ...string) int64 {
	for _, k := range keys {
		if _, ok := m[k]; ok {
			return num(m, k)
		}
	}
	return 0
}
