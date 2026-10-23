package gemini

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/dillonzq/apix/internal/errclass"
	"github.com/dillonzq/apix/internal/llm/adapter/shared"
)

type streamCall struct {
	id, name, args string
	initial        object
	emitted        bool
	ready          bool
}

type streamCandidate struct {
	parts     []any
	finish    string
	grounding object
	started   bool
	calls     map[string]*streamCall
	order     []string
	text      map[string]string
}

type streamBlock struct {
	kind string
	call *streamCall
}

// StreamConverter preserves incremental content while retaining final usage and
// signatures. A Gemini finishReason is a semantic terminal, not transport EOF.
// Later signature/usage frames are processed before Finish renders the terminal.
type StreamConverter struct {
	from, to      string
	ctx           *shared.ConversionContext
	framer        *shared.SSEFramer
	failure       *errclass.Error
	done, started bool
	id, model     string
	usage         object
	candidates    map[int]*streamCandidate
	order         []int
	blocks        map[int]*streamBlock
	seq           int64
	created       int64
	activePart    int
	activeKind    string
	itemID        string
}

func NewStreamConverter(from, to string, ctx *shared.ConversionContext) *StreamConverter {
	return &StreamConverter{from: from, to: to, ctx: shared.ContextOrNew([]*shared.ConversionContext{ctx}), framer: shared.NewSSEFramer(true), created: time.Now().Unix(), candidates: map[int]*streamCandidate{}, blocks: map[int]*streamBlock{}, usage: object{}, activePart: -1}
}

func (s *StreamConverter) candidate(index int) *streamCandidate {
	c := s.candidates[index]
	if c == nil {
		c = &streamCandidate{calls: map[string]*streamCall{}, text: map[string]string{}}
		s.candidates[index] = c
		s.order = append(s.order, index)
	}
	return c
}

func (s *StreamConverter) converter() *conversion {
	return &conversion{from: s.from, to: s.to, ctx: s.ctx, response: true}
}

func (s *StreamConverter) Feed(chunk []byte) ([][]byte, bool, *errclass.Error) {
	if s.failure != nil {
		return nil, false, s.failure
	}
	if s.done {
		return nil, true, nil
	}
	s.framer.Push(chunk)
	events, err := s.drain()
	if err != nil {
		s.failure = err
	}
	return events, s.done && err == nil, err
}

func (s *StreamConverter) drain() ([][]byte, *errclass.Error) {
	var events [][]byte
	for {
		name, data, raw, ok := s.framer.Next()
		if !ok {
			return events, nil
		}
		if data == "" {
			if s.from == Format && s.to == Format {
				events = append(events, raw)
			}
			continue
		}
		if strings.TrimSpace(data) == "[DONE]" {
			if s.from != chat {
				return events, errclass.Translation("unexpected stream done sentinel")
			}
			for _, i := range s.order {
				if s.candidates[i].finish == "" {
					s.candidates[i].finish = "STOP"
				}
			}
			tail, err := s.complete()
			events = append(events, tail...)
			return events, err
		}
		root, err := decode([]byte(data))
		if err != nil {
			return events, err
		}
		if err := responseError(root, []byte(data)); err != nil {
			return events, err
		}
		var out [][]byte
		if s.from == Format {
			out, err = s.geminiChunk(root, raw)
		} else {
			out, err = s.otherChunk(name, root)
		}
		events = append(events, out...)
		if err != nil || s.done {
			return events, err
		}
	}
}

func (s *StreamConverter) Finish() ([][]byte, *errclass.Error) {
	if s.failure != nil {
		return nil, s.failure
	}
	if s.done {
		return nil, nil
	}
	s.framer.End()
	out, err := s.drain()
	if err == nil && !s.done {
		var tail [][]byte
		tail, err = s.complete()
		out = append(out, tail...)
	}
	if err != nil {
		s.failure = err
	}
	return out, err
}

func (s *StreamConverter) identity(id, model string) {
	if !s.started && id != "" {
		s.id = id
	}
	if model != "" {
		s.model = model
	}
}

func (s *StreamConverter) event(name string, doc object) []byte {
	doc["type"] = name
	if s.to == responses {
		doc["sequence_number"] = s.seq
		s.seq++
	}
	return shared.SSEEvent(name, doc)
}

func (s *StreamConverter) start() [][]byte {
	if s.started {
		return nil
	}
	s.started = true
	if s.id == "" {
		s.id = "apix"
	}
	switch s.to {
	case messages:
		return [][]byte{s.event("message_start", object{"message": object{"id": s.id, "type": "message", "role": "assistant", "model": s.model, "content": []any{}, "usage": renderUsage(s.usage, messages)}})}
	case responses:
		return [][]byte{s.event("response.created", object{"response": object{"id": s.id, "object": "response", "status": "in_progress", "model": s.model, "output": []any{}}})}
	}
	return nil
}

func (s *StreamConverter) chatChunk(index int, delta object, reason any, usage object) []byte {
	out := object{"id": s.id, "object": "chat.completion.chunk", "created": s.created, "model": s.model, "choices": []any{object{"index": index, "delta": delta, "finish_reason": reason}}}
	if usage != nil {
		out["usage"] = usage
	}
	return dataFrame(out)
}

func (s *StreamConverter) geminiChunk(root object, raw []byte) ([][]byte, *errclass.Error) {
	normalizer := s.converter()
	normalizer.normalizeGemini(root)
	normalizer.object(root["usageMetadata"], "usageMetadata")
	normalizer.object(root["promptFeedback"], "promptFeedback")
	for _, cv := range normalizer.array(root["candidates"], "candidates") {
		candidate := normalizer.object(cv, "candidate")
		if candidate == nil {
			normalizer.fail("candidates must contain objects")
			continue
		}
		normalizer.numeric(candidate["index"], "candidate.index", true)
		if number(candidate["index"]) < 0 {
			normalizer.fail("candidate.index must not be negative")
		}
		normalizer.text(candidate["finishReason"], "finishReason")
		content := normalizer.object(candidate["content"], "content")
		for _, pv := range normalizer.array(content["parts"], "parts") {
			part := normalizer.object(pv, "part")
			if part == nil {
				normalizer.fail("parts must contain objects")
				continue
			}
			normalizer.text(part["text"], "part.text")
			normalizer.flags(part["thought"], "part.thought")
			normalizer.text(part["thoughtSignature"], "thoughtSignature")
		}
	}
	if normalizer.err != nil {
		return nil, normalizer.err
	}
	s.identity(str(root["responseId"]), str(root["modelVersion"]))
	if u := obj(root["usageMetadata"]); u != nil {
		for k, v := range u {
			s.usage[k] = v
		}
	}
	var out [][]byte
	candidates := list(root["candidates"])
	if root["candidates"] != nil && candidates == nil {
		return nil, errclass.Translation("candidates must be an array")
	}
	if feedback := obj(root["promptFeedback"]); str(feedback["blockReason"]) != "" {
		candidates = []any{object{"index": 0, "content": object{"parts": []any{object{"text": "Request blocked by Gemini."}}}, "finishReason": "SAFETY"}}
	}
	for ci, cv := range candidates {
		candidate := obj(cv)
		if candidate == nil {
			return out, errclass.Translation("candidate must be an object")
		}
		index := ci
		if candidate["index"] != nil {
			index = int(number(candidate["index"]))
		}
		state := s.candidate(index)
		if reason := str(candidate["finishReason"]); reason != "" {
			if reason == "MALFORMED_FUNCTION_CALL" {
				return out, errclass.UpstreamFallback("Gemini produced a malformed function call")
			}
			state.finish = reason
		}
		if s.to != chat && s.to != Format && index != s.order[0] {
			s.ctx.Report(Format, s.to, "candidates[1:]", true)
			continue
		}
		content := obj(candidate["content"])
		parts := list(content["parts"])
		if content["parts"] != nil && parts == nil {
			return out, errclass.Translation("parts must be an array")
		}
		for _, pv := range parts {
			part := obj(pv)
			if part == nil {
				return out, errclass.Translation("part must be an object")
			}
			if s.to == Format {
				state.parts = append(state.parts, part)
				continue
			}
			conv := s.converter()
			conv.validateParts([]any{part})
			if conv.err != nil {
				return out, conv.err
			}
			out = append(out, s.start()...)
			if s.to == chat && !state.started {
				out = append(out, s.chatChunk(index, object{"role": "assistant", "content": ""}, nil, nil))
				state.started = true
			}
			frames, err := s.geminiPart(index, state, part)
			out = append(out, frames...)
			if err != nil {
				return out, err
			}
		}
		if candidate["groundingMetadata"] != nil {
			state.grounding = obj(candidate["groundingMetadata"])
		}
	}
	if s.to == Format {
		return [][]byte{raw}, nil
	}
	return out, nil
}

func partKind(p object) string {
	if p["functionCall"] != nil {
		return "tool"
	}
	if p["text"] != nil {
		if boolean(p["thought"]) {
			return "thinking"
		}
		return "text"
	}
	return "media"
}

func (s *StreamConverter) geminiPart(index int, state *streamCandidate, part object) ([][]byte, *errclass.Error) {
	if part["text"] == nil && part["functionCall"] == nil && signature(part) != "" && len(part) == 1 {
		if len(state.parts) > 0 {
			obj(state.parts[len(state.parts)-1])["thoughtSignature"] = signature(part)
		} else {
			s.ctx.Report(Format, s.to, "stream.orphan_thought_signature", true)
		}
		return nil, nil
	}
	kind := partKind(part)
	merge := false
	last := len(state.parts) - 1
	if last >= 0 && (kind == "text" || kind == "thinking") {
		prev := obj(state.parts[last])
		merge = partKind(prev) == kind && signature(prev) == ""
	}
	var out [][]byte
	if !merge {
		if s.to != chat {
			closed, err := s.closePart(state)
			out = append(out, closed...)
			if err != nil {
				return out, err
			}
		}
		state.parts = append(state.parts, clone(part))
		last = len(state.parts) - 1
		if s.to != chat {
			s.activePart = last
			s.activeKind = kind
			s.itemID = fmt.Sprintf("%s_item_%d", s.id, last)
		}
	} else {
		prev := obj(state.parts[last])
		prev["text"] = str(prev["text"]) + str(part["text"])
		if signature(part) != "" {
			prev["thoughtSignature"] = signature(part)
		}
	}
	conv := s.converter()
	switch s.to {
	case chat:
		delta := object{}
		switch kind {
		case "text":
			delta["content"] = part["text"]
		case "thinking":
			delta["reasoning_content"] = part["text"]
		case "tool":
			fc := obj(part["functionCall"])
			args := conv.args(fc["args"])
			delta["tool_calls"] = []any{object{"index": countCalls(state.parts) - 1, "id": callID(fc, last), "type": "function", "function": object{"name": fc["name"], "arguments": string(encode(args))}}}
		default:
			media := conv.fromMedia(part)
			if media != nil {
				delta["images"] = []any{media}
			}
		}
		if !s.started {
			s.started = true
		}
		out = append(out, s.chatChunk(index, delta, nil, nil))
	case messages:
		if !merge {
			block := object{"type": "text", "text": ""}
			switch kind {
			case "thinking":
				block = object{"type": "thinking", "thinking": ""}
			case "tool":
				fc := obj(part["functionCall"])
				block = object{"type": "tool_use", "id": callID(fc, last), "name": fc["name"], "input": object{}}
			case "media":
				block = conv.fromMedia(part)
			}
			out = append(out, s.event("content_block_start", object{"index": last, "content_block": block}))
		}
		delta := object{}
		switch kind {
		case "text":
			delta = object{"type": "text_delta", "text": str(part["text"])}
		case "thinking":
			delta = object{"type": "thinking_delta", "thinking": str(part["text"])}
		case "tool":
			delta = object{"type": "input_json_delta", "partial_json": string(encode(conv.args(obj(part["functionCall"])["args"])))}
		}
		if len(delta) > 0 {
			out = append(out, s.event("content_block_delta", object{"index": last, "delta": delta}))
		}
	case responses:
		_, _, items, _, _ := conv.renderedParts([]any{part}, last)
		if conv.err != nil {
			return out, conv.err
		}
		if len(items) != 1 {
			return out, unsupported("unsupported Gemini stream part for Responses")
		}
		item := obj(items[0])
		item["id"] = s.itemID
		typ := str(item["type"])
		if !merge {
			announced := clone(item)
			switch typ {
			case "message":
				announced["content"] = []any{}
			case "reasoning":
				announced["summary"] = []any{}
			case "function_call":
				announced["arguments"] = ""
			case "custom_tool_call":
				announced["input"] = ""
			}
			announced["status"] = "in_progress"
			out = append(out, s.event("response.output_item.added", object{"output_index": last, "item": announced}))
			if typ == "message" {
				out = append(out, s.event("response.content_part.added", object{"item_id": s.itemID, "output_index": last, "content_index": 0, "part": object{"type": "output_text", "text": "", "annotations": []any{}}}))
			}
			if typ == "reasoning" {
				out = append(out, s.event("response.reasoning_summary_part.added", object{"item_id": s.itemID, "output_index": last, "summary_index": 0, "part": object{"type": "summary_text", "text": ""}}))
			}
		}
		name := ""
		delta := ""
		switch typ {
		case "message":
			name = "response.output_text.delta"
			delta = str(part["text"])
		case "reasoning":
			name = "response.reasoning_summary_text.delta"
			delta = str(part["text"])
		case "function_call":
			name = "response.function_call_arguments.delta"
			delta = str(item["arguments"])
		case "custom_tool_call":
			name = "response.custom_tool_call_input.delta"
			delta = str(item["input"])
		}
		if name != "" {
			out = append(out, s.event(name, object{"item_id": s.itemID, "output_index": last, "content_index": 0, "summary_index": 0, "delta": delta}))
		}
	}
	return out, conv.err
}

func countCalls(parts []any) int {
	n := 0
	for _, pv := range parts {
		if obj(pv)["functionCall"] != nil {
			n++
		}
	}
	return n
}

func (s *StreamConverter) closePart(state *streamCandidate) ([][]byte, *errclass.Error) {
	if s.activePart < 0 {
		return nil, nil
	}
	index := s.activePart
	s.activePart = -1
	if s.to == messages {
		return [][]byte{s.event("content_block_stop", object{"index": index})}, nil
	}
	if s.to != responses {
		return nil, nil
	}
	part := obj(state.parts[index])
	conv := s.converter()
	_, _, items, _, _ := conv.renderedParts([]any{part}, index)
	if conv.err != nil {
		return nil, conv.err
	}
	if len(items) != 1 {
		return nil, unsupported("unsupported Gemini stream item")
	}
	item := obj(items[0])
	item["id"] = s.itemID
	item["status"] = "completed"
	var out [][]byte
	switch str(item["type"]) {
	case "message":
		out = append(out, s.event("response.output_text.done", object{"item_id": s.itemID, "output_index": index, "content_index": 0, "text": str(part["text"])}))
		out = append(out, s.event("response.content_part.done", object{"item_id": s.itemID, "output_index": index, "content_index": 0, "part": list(item["content"])[0]}))
	case "reasoning":
		out = append(out, s.event("response.reasoning_summary_text.done", object{"item_id": s.itemID, "output_index": index, "summary_index": 0, "text": str(part["text"])}))
		out = append(out, s.event("response.reasoning_summary_part.done", object{"item_id": s.itemID, "output_index": index, "summary_index": 0, "part": list(item["summary"])[0]}))
	case "function_call":
		out = append(out, s.event("response.function_call_arguments.done", object{"item_id": s.itemID, "output_index": index, "arguments": item["arguments"]}))
	case "custom_tool_call":
		out = append(out, s.event("response.custom_tool_call_input.done", object{"item_id": s.itemID, "output_index": index, "input": item["input"]}))
	}
	out = append(out, s.event("response.output_item.done", object{"output_index": index, "item": item}))
	return out, nil
}

func (s *StreamConverter) emitGemini(index int, parts []any) [][]byte {
	if len(parts) == 0 {
		return nil
	}
	s.start()
	state := s.candidate(index)
	state.parts = append(state.parts, parts...)
	return [][]byte{dataFrame(object{"responseId": s.id, "modelVersion": s.model, "candidates": []any{object{"index": index, "content": object{"role": "model", "parts": parts}}}})}
}

func (s *StreamConverter) appendText(index int, key, text string, thought, snapshot bool) [][]byte {
	if text == "" {
		return nil
	}
	state := s.candidate(index)
	prior := state.text[key]
	if snapshot {
		if strings.HasPrefix(text, prior) {
			text = strings.TrimPrefix(text, prior)
		} else if prior != "" {
			s.ctx.Report(s.from, Format, "stream.text_snapshot", true)
			return nil
		}
	}
	state.text[key] = prior + text
	if text == "" {
		return nil
	}
	p := object{"text": text}
	if thought {
		p["thought"] = true
	}
	return s.emitGemini(index, []any{p})
}

func (s *StreamConverter) call(index int, key string) *streamCall {
	state := s.candidate(index)
	call := state.calls[key]
	if call == nil {
		call = &streamCall{}
		state.calls[key] = call
		state.order = append(state.order, key)
	}
	return call
}

func (s *StreamConverter) otherChunk(name string, root object) ([][]byte, *errclass.Error) {
	var out [][]byte
	if name == "" {
		name = str(root["type"])
	}
	switch s.from {
	case chat:
		if err := shared.CheckProtocolFailure(encode(root), chat); err != nil {
			return nil, err
		}
		s.identity(str(root["id"]), str(root["model"]))
		if u := obj(root["usage"]); u != nil {
			for k, v := range u {
				s.usage[k] = v
			}
		}
		choices := list(root["choices"])
		if root["choices"] != nil && choices == nil {
			return nil, errclass.Translation("choices must be an array")
		}
		for ci, cv := range choices {
			ch := obj(cv)
			index := ci
			if ch["index"] != nil {
				index = int(number(ch["index"]))
			}
			state := s.candidate(index)
			d := obj(ch["delta"])
			out = append(out, s.appendText(index, "text", str(d["content"]), false, false)...)
			out = append(out, s.appendText(index, "thought", str(d["reasoning_content"]), true, false)...)
			for _, tv := range list(d["tool_calls"]) {
				t := obj(tv)
				key := strconv.FormatInt(number(t["index"]), 10)
				call := s.call(index, key)
				if id := str(t["id"]); id != "" {
					call.id = id
				}
				f := obj(t["function"])
				if n := str(f["name"]); n != "" {
					call.name += n
				}
				call.args += str(f["arguments"])
			}
			if reason := str(ch["finish_reason"]); reason != "" {
				state.finish = geminiFinish(reason)
			}
		}
	case messages:
		if name == "error" {
			return nil, shared.UpstreamStatusError(502, encode(root))
		}
		index := int(number(root["index"]))
		switch name {
		case "message_start":
			m := obj(root["message"])
			s.identity(str(m["id"]), str(m["model"]))
			for k, v := range obj(m["usage"]) {
				s.usage[k] = v
			}
			s.candidate(0)
		case "content_block_start":
			b := obj(root["content_block"])
			kind := str(b["type"])
			block := &streamBlock{kind: kind}
			s.blocks[index] = block
			key := strconv.Itoa(index)
			switch kind {
			case "text":
				out = append(out, s.appendText(0, key, str(b["text"]), false, false)...)
			case "thinking":
				out = append(out, s.appendText(0, key, str(b["thinking"]), true, false)...)
			case "tool_use":
				call := s.call(0, key)
				call.id, call.name, call.initial = str(b["id"]), str(b["name"]), obj(b["input"])
				block.call = call
			case "redacted_thinking":
				s.ctx.Report(messages, Format, "content.redacted_thinking", true)
			default:
				return out, unsupported("unsupported Messages stream content block")
			}
		case "content_block_delta":
			b := s.blocks[index]
			if b == nil {
				return out, errclass.Translation("delta has no content_block_start")
			}
			d := obj(root["delta"])
			key := strconv.Itoa(index)
			switch str(d["type"]) {
			case "text_delta":
				out = append(out, s.appendText(0, key, str(d["text"]), false, false)...)
			case "thinking_delta":
				out = append(out, s.appendText(0, key, str(d["thinking"]), true, false)...)
			case "input_json_delta":
				if b.call == nil {
					return out, errclass.Translation("tool delta has no tool block")
				}
				b.call.args += str(d["partial_json"])
			case "signature_delta":
				s.ctx.Report(messages, Format, "content.thinking.signature", true)
			case "citations_delta":
				s.ctx.Report(messages, Format, "stream.citations", false)
			default:
				return out, unsupported("unsupported Messages content delta")
			}
		case "content_block_stop":
			if block := s.blocks[index]; block != nil && block.call != nil {
				block.call.ready = true
			}
			frames, err := s.flushCalls(0, false)
			out = append(out, frames...)
			if err != nil {
				return out, err
			}
		case "message_delta":
			if reason := str(obj(root["delta"])["stop_reason"]); reason != "" {
				s.candidate(0).finish = geminiFinish(reason)
			}
			for k, v := range obj(root["usage"]) {
				s.usage[k] = v
			}
		case "message_stop":
			if s.candidate(0).finish == "" {
				s.candidate(0).finish = "STOP"
			}
			tail, err := s.complete()
			out = append(out, tail...)
			return out, err
		case "ping":
		default:
			return out, unsupported("unsupported Messages stream event")
		}
	case responses:
		switch name {
		case "response.created", "response.in_progress":
			r := obj(root["response"])
			s.identity(str(r["id"]), str(r["model"]))
			s.candidate(0)
		case "response.output_item.added":
			item := obj(root["item"])
			if str(item["type"]) == "function_call" {
				key := str(item["id"])
				if key == "" {
					key = strconv.FormatInt(number(root["output_index"]), 10)
				}
				call := s.call(0, key)
				call.id, call.name, call.args = str(item["call_id"]), str(item["name"]), str(item["arguments"])
			}
		case "response.output_text.delta", "response.refusal.delta", "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
			key := str(root["item_id"]) + ":" + strconv.FormatInt(number(root["content_index"]), 10)
			thought := strings.Contains(name, "reasoning")
			if thought {
				key += "thought"
			}
			out = append(out, s.appendText(0, key, str(root["delta"]), thought, false)...)
		case "response.function_call_arguments.delta":
			s.call(0, str(root["item_id"])).args += str(root["delta"])
		case "response.function_call_arguments.done":
			call := s.call(0, str(root["item_id"]))
			if args := str(root["arguments"]); args != "" {
				if call.args != "" && !strings.HasPrefix(args, call.args) {
					return out, errclass.Translation("inconsistent function argument snapshot")
				}
				call.args = args
			}
		case "response.output_item.done":
			frames, err := s.responsesItem(obj(root["item"]))
			out = append(out, frames...)
			if err != nil {
				return out, err
			}
		case "response.completed", "response.incomplete":
			r := obj(root["response"])
			if r == nil || str(r["id"]) == "" || r["output"] == nil {
				return out, errclass.Translation("invalid Responses terminal")
			}
			s.identity(str(r["id"]), str(r["model"]))
			for _, iv := range list(r["output"]) {
				frames, err := s.responsesItem(obj(iv))
				out = append(out, frames...)
				if err != nil {
					return out, err
				}
			}
			for k, v := range obj(r["usage"]) {
				s.usage[k] = v
			}
			s.candidate(0).finish = "STOP"
			if name == "response.incomplete" {
				s.candidate(0).finish = "MAX_TOKENS"
			}
			tail, err := s.complete()
			out = append(out, tail...)
			return out, err
		case "response.failed", "error":
			return out, shared.UpstreamStatusError(502, encode(root))
		case "response.content_part.added", "response.content_part.done", "response.output_text.done", "response.refusal.done", "response.reasoning_summary_part.added", "response.reasoning_summary_part.done", "response.reasoning_summary_text.done", "response.reasoning_text.done":
		default:
			if strings.Contains(name, "annotation") {
				s.ctx.Report(responses, Format, "stream.annotations", false)
			} else {
				return out, unsupported("unsupported Responses stream event")
			}
		}
	default:
		return out, unsupported("unsupported Gemini stream source")
	}
	return out, nil
}

func (s *StreamConverter) responsesItem(item object) ([][]byte, *errclass.Error) {
	var out [][]byte
	id := str(item["id"])
	switch str(item["type"]) {
	case "message":
		for i, pv := range list(item["content"]) {
			p := obj(pv)
			key := id + ":" + strconv.Itoa(i)
			switch str(p["type"]) {
			case "output_text":
				out = append(out, s.appendText(0, key, str(p["text"]), false, true)...)
			case "refusal":
				out = append(out, s.appendText(0, key, str(p["refusal"]), false, true)...)
			default:
				return out, unsupported("unsupported Responses message content")
			}
		}
	case "reasoning":
		values := list(item["content"])
		if values == nil {
			values = list(item["summary"])
		}
		var text strings.Builder
		for _, v := range values {
			text.WriteString(str(obj(v)["text"]))
		}
		out = append(out, s.appendText(0, id+":0thought", text.String(), true, true)...)
	case "function_call":
		if id == "" {
			for _, key := range s.candidate(0).order {
				if s.candidate(0).calls[key].id == str(item["call_id"]) {
					id = key
					break
				}
			}
		}
		if id == "" {
			id = str(item["call_id"])
		}
		call := s.call(0, id)
		call.ready = true
		if call.emitted {
			return nil, nil
		}
		if name := str(item["name"]); name != "" {
			call.name = name
		}
		if callID := str(item["call_id"]); callID != "" {
			call.id = callID
		}
		if args := str(item["arguments"]); args != "" {
			if call.args != "" && !strings.HasPrefix(args, call.args) {
				return out, errclass.Translation("inconsistent tool argument snapshot")
			}
			call.args = args
		}
		frames, err := s.flushCalls(0, false)
		return append(out, frames...), err
	default:
		return out, unsupported("unsupported actual Responses output item")
	}
	return out, nil
}

func (s *StreamConverter) flushCalls(index int, final bool) ([][]byte, *errclass.Error) {
	state := s.candidate(index)
	var out [][]byte
	for _, key := range state.order {
		call := state.calls[key]
		if call.emitted || !final && !call.ready {
			continue
		}
		if call.name == "" {
			if final {
				return out, errclass.Translation("streamed tool has no name")
			}
			continue
		}
		var args object
		if call.args == "" {
			args = call.initial
			if args == nil {
				args = object{}
			}
		} else {
			var err *errclass.Error
			args, err = decode([]byte(call.args))
			if err != nil {
				if final {
					return out, errclass.Translation("incomplete streamed tool arguments")
				}
				continue
			}
		}
		id := call.id
		if id == "" {
			id = "call_" + key
		}
		out = append(out, s.emitGemini(index, []any{object{"functionCall": object{"id": id, "name": call.name, "args": args}}})...)
		call.emitted = true
	}
	return out, nil
}

func (s *StreamConverter) complete() ([][]byte, *errclass.Error) {
	if s.done {
		return nil, nil
	}
	if len(s.order) == 0 {
		return nil, errclass.Translation("stream ended without a terminal state")
	}
	for _, i := range s.order {
		if s.candidates[i].finish == "" {
			return nil, errclass.Translation("stream ended without a terminal state")
		}
	}
	var out [][]byte
	if s.to == Format && s.from == Format {
		s.done = true
		return nil, nil
	}
	if s.to == Format {
		for _, i := range s.order {
			frames, err := s.flushCalls(i, true)
			out = append(out, frames...)
			if err != nil {
				return out, err
			}
			state := s.candidates[i]
			out = append(out, dataFrame(object{"responseId": s.id, "modelVersion": s.model, "candidates": []any{object{"index": i, "content": object{"role": "model", "parts": []any{}}, "finishReason": state.finish}}, "usageMetadata": usageToGemini(s.usage, s.from)}))
		}
	} else {
		out = append(out, s.start()...)
		for _, i := range s.order {
			state := s.candidates[i]
			if s.to != chat && i != s.order[0] {
				continue
			}
			if s.to != chat {
				out = append(out, s.finishCitations(state)...)
				closed, err := s.closePart(state)
				out = append(out, closed...)
				if err != nil {
					return out, err
				}
			}
			reason := finish(state.finish, countCalls(state.parts) > 0)
			switch s.to {
			case chat:
				if !state.started {
					out = append(out, s.chatChunk(i, object{"role": "assistant", "content": ""}, nil, nil))
					state.started = true
				}
				out = append(out, s.chatChunk(i, s.finalChatDelta(state), reason, renderUsage(s.usage, chat)))
			case messages:
				stop := "end_turn"
				switch reason {
				case "length":
					stop = "max_tokens"
				case "tool_calls":
					stop = "tool_use"
				case "content_filter":
					stop = "refusal"
				}
				out = append(out, s.event("message_delta", object{"delta": object{"stop_reason": stop, "stop_sequence": nil, "extra_content": carrier(state.parts)}, "usage": renderUsage(s.usage, messages)}), s.event("message_stop", object{}))
			case responses:
				root := object{"responseId": s.id, "modelVersion": s.model, "candidates": []any{object{"content": object{"parts": state.parts}, "finishReason": state.finish, "groundingMetadata": state.grounding}}, "usageMetadata": s.usage}
				conv := s.converter()
				response := conv.renderResponse(root)
				if conv.err != nil {
					return out, conv.err
				}
				for idx, iv := range list(response["output"]) {
					obj(iv)["id"] = fmt.Sprintf("%s_item_%d", s.id, idx)
				}
				name := "response.completed"
				if reason == "length" {
					name = "response.incomplete"
				}
				out = append(out, s.event(name, object{"response": response}))
			}
		}
		if s.to == chat {
			out = append(out, []byte("data: [DONE]\n\n"))
		}
	}
	s.done = true
	return out, nil
}

func (s *StreamConverter) finishCitations(state *streamCandidate) [][]byte {
	candidate := object{"content": object{"parts": state.parts}, "groundingMetadata": state.grounding}
	var out [][]byte
	indices := make(map[int]int)
	for _, citation := range s.converter().groundingCitations(candidate) {
		offset := 0
		placed := false
		for pi, pv := range state.parts {
			part := obj(pv)
			if boolean(part["thought"]) {
				continue
			}
			length := len([]rune(str(part["text"])))
			if citation.Start != nil && citation.End != nil && *citation.Start >= offset && *citation.End <= offset+length {
				if s.to == messages && pi == s.activePart && s.activeKind == "text" {
					out = append(out, s.event("content_block_delta", object{"index": pi, "delta": object{"type": "citations_delta", "citation": citation.Wire(messages)}}))
					placed = true
				} else if s.to == responses {
					a, b := *citation.Start-offset, *citation.End-offset
					citation.Start, citation.End = &a, &b
					out = append(out, s.event("response.output_text.annotation.added", object{"item_id": fmt.Sprintf("%s_item_%d", s.id, pi), "output_index": pi, "content_index": 0, "annotation_index": indices[pi], "annotation": citation.Wire(responses)}))
					indices[pi]++
					placed = true
				}
				break
			}
			offset += length
		}
		if !placed {
			s.ctx.Report(Format, s.to, "groundingMetadata.closed_block", false)
		}
	}
	return out
}

func (s *StreamConverter) finalChatDelta(state *streamCandidate) object {
	delta := object{"extra_content": carrier(state.parts)}
	candidate := object{"content": object{"parts": state.parts}, "groundingMetadata": state.grounding}
	annotations := s.converter().applyCitations(candidate, nil, nil)
	if len(annotations) > 0 {
		delta["annotations"] = annotations
	}
	return delta
}
