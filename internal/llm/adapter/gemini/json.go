// Package gemini implements direct GenerateContent conversions. Gemini wire
// objects are the source or destination, never an intermediate protocol.
package gemini

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/dillonzq/apix/internal/errclass"
	"github.com/dillonzq/apix/internal/llm/adapter/shared"
)

// Format is the internal format name of GenerateContent.
const Format = shared.FormatGemini

const (
	chat      = shared.FormatChatCompletions
	messages  = shared.FormatMessages
	responses = shared.FormatResponses
)

type object = map[string]any

func decode(body []byte) (object, *errclass.Error) {
	d := json.NewDecoder(bytes.NewReader(body))
	d.UseNumber()
	var out object
	if err := d.Decode(&out); err != nil || out == nil {
		return nil, errclass.Translation("expected a JSON object")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return nil, errclass.Translation("unexpected trailing JSON")
	}
	return out, nil
}

func encode(v any) []byte { b, _ := json.Marshal(v); return b }
func obj(v any) object    { o, _ := v.(map[string]any); return o }
func list(v any) []any    { a, _ := v.([]any); return a }
func str(v any) string    { s, _ := v.(string); return s }
func boolean(v any) bool  { b, _ := v.(bool); return b }
func number(v any) int64 {
	switch n := v.(type) {
	case json.Number:
		i, _ := n.Int64()
		return i
	case int:
		return int64(n)
	case int64:
		return n
	}
	return 0
}

func first(o object, keys ...string) any {
	for _, k := range keys {
		if v, ok := o[k]; ok {
			return v
		}
	}
	return nil
}

func clone(o object) object {
	r := object{}
	for k, v := range o {
		r[k] = v
	}
	return r
}

func stringOrJSON(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return string(encode(v))
}

func dataFrame(v any) []byte { return append(append([]byte("data: "), encode(v)...), '\n', '\n') }
func unsupported(message string) *errclass.Error {
	return &errclass.Error{Class: errclass.ClassUnsupported, Message: message}
}

type conversion struct {
	from, to string
	ctx      *shared.ConversionContext
	err      *errclass.Error
	response bool
	native   bool
}

func (c *conversion) fail(message string) {
	if c.err == nil {
		c.err = errclass.Translation(message)
	}
}

func (c *conversion) reject(message string) {
	if c.err == nil {
		c.err = unsupported(message)
	}
}

func (c *conversion) object(v any, path string) object {
	if v == nil {
		return nil
	}
	o := obj(v)
	if o == nil {
		c.fail(path + " must be an object")
	}
	return o
}

func (c *conversion) array(v any, path string) []any {
	if v == nil {
		return nil
	}
	a, ok := v.([]any)
	if !ok {
		c.fail(path + " must be an array")
	}
	return a
}

func (c *conversion) text(v any, path string) string {
	if v == nil {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		c.fail(path + " must be a string")
	}
	return s
}

func (c *conversion) loss(path string, semantic bool) {
	if c.response {
		c.ctx.Report(c.from, c.to, path, semantic)
		return
	}
	if err := c.ctx.RejectLoss(shared.DiagnosticFor(c.from, c.to, path, semantic)); err != nil && c.err == nil {
		c.err = err
	}
}

func (c *conversion) unknown(o object, path string, known ...string) {
	if c.native {
		return
	}
	allowed := make(map[string]bool, len(known))
	for _, k := range known {
		allowed[k] = true
	}
	keys := make([]string, 0, len(o))
	for k, v := range o {
		if v != nil && !allowed[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		c.loss(path+k, k != "metadata" && k != "user" && k != "stream_options")
	}
}

func (c *conversion) args(v any) object {
	if v == nil {
		return object{}
	}
	if s, ok := v.(string); ok {
		if strings.TrimSpace(s) == "" {
			return object{}
		}
		o, err := decode([]byte(s))
		if err != nil {
			c.fail("function arguments must be a JSON object")
		}
		return o
	}
	o := c.object(v, "function arguments")
	if o == nil {
		return object{}
	}
	return o
}

func callID(fc object, index int) string {
	if id := str(fc["id"]); id != "" {
		return id
	}
	return fmt.Sprintf("call_gemini_%d", index)
}

// A carrier retains the original signed parts, including their order. Hosts
// must preserve it in conversation history; it is never a provider signature.
func carrier(parts []any) object   { return object{"google": object{"parts": parts}} }
func carrierParts(o object) []any  { return list(obj(obj(o["extra_content"])["google"])["parts"]) }
func signature(part object) string { return str(first(part, "thoughtSignature", "thought_signature")) }
func signatureFrom(o object) string {
	return str(obj(obj(o["extra_content"])["google"])["thought_signature"])
}

func (c *conversion) validateParts(parts []any) {
	for _, v := range parts {
		p := c.object(v, "parts[]")
		if p == nil {
			c.fail("parts must contain objects")
			continue
		}
		kinds := 0
		for _, key := range []string{"text", "functionCall", "functionResponse", "inlineData", "fileData"} {
			if p[key] != nil {
				kinds++
			}
		}
		if kinds > 1 {
			c.fail("a Gemini part must have one content variant")
		}
		if t, ok := p["text"]; ok {
			c.text(t, "part.text")
		}
		if sig := first(p, "thoughtSignature", "thought_signature"); sig != nil {
			c.text(sig, "thoughtSignature")
		}
		if t := p["thought"]; t != nil {
			if _, ok := t.(bool); !ok {
				c.fail("thought must be a boolean")
			}
		}
		for _, key := range []string{"functionCall", "functionResponse"} {
			if p[key] != nil {
				f := c.object(p[key], key)
				if c.text(f["name"], key+".name") == "" {
					c.fail(key + " requires a name")
				}
				if f["id"] != nil {
					c.text(f["id"], key+".id")
				}
				if key == "functionCall" {
					if f["partialArgs"] != nil || f["partial_args"] != nil || boolean(f["willContinue"]) {
						c.reject("incremental Gemini function arguments require complete args snapshots")
					}
					c.args(f["args"])
				} else {
					c.object(f["response"], "functionResponse.response")
				}
			}
		}
		c.unknown(p, "parts[].", "text", "thought", "thoughtSignature", "thought_signature", "functionCall", "functionResponse", "inlineData", "fileData", "videoMetadata")
	}
}
