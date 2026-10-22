package messages

import (
	"encoding/json"
	"fmt"

	"github.com/dillonzq/apix/internal/errclass"
	"github.com/dillonzq/apix/internal/llm/adapter/shared"
	"github.com/dillonzq/apix/thinking"
)

type anthropicBlock = map[string]any

func textBlock(text string) anthropicBlock {
	return anthropicBlock{"type": "text", "text": text}
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"` // string or []anthropicBlock
}

type anthropicTool struct {
	CacheControl   json.RawMessage `json:"cache_control,omitempty"`
	Name           string          `json:"name"`
	Description    string          `json:"description,omitempty"`
	InputSchema    json.RawMessage `json:"input_schema,omitempty"`
	Strict         *bool           `json:"strict,omitempty"`
	Type           string          `json:"type,omitempty"`
	UserLocation   json.RawMessage `json:"user_location,omitempty"`
	AllowedDomains json.RawMessage `json:"allowed_domains,omitempty"`
}

// messagesRequest is the outbound Anthropic encode shape — intentionally
// divergent from shared.ClaudeTool decode type: typed stop sequences +
// named items vs raw-ignore + tool_choice.
type messagesRequest struct {
	Model         string                 `json:"model"`
	MaxTokens     int64                  `json:"max_tokens"`
	System        any                    `json:"system,omitempty"` // string or []anthropicBlock
	Messages      []anthropicMessage     `json:"messages"`
	StopSequences []string               `json:"stop_sequences,omitempty"`
	Tools         []anthropicTool        `json:"tools,omitempty"`
	ToolChoice    any                    `json:"tool_choice,omitempty"` // {type,name?,disable_parallel_tool_use?}
	Thinking      *shared.ClaudeThinking `json:"thinking,omitempty"`
	Stream        bool                   `json:"stream,omitempty"`
	Temperature   *float64               `json:"temperature,omitempty"`
	TopP          *float64               `json:"top_p,omitempty"`
	OutputConfig  json.RawMessage        `json:"output_config,omitempty"`
}

// BuildRequest translates an inbound request body from sourceFormat
// ("chat-completions" Chat Completions, "responses" Responses, "messages"
// passthrough) into an Anthropic Messages body for upstreamModel. Reasoning controls
// are not filtered or clamped.
// Unknown formats are ClassUnsupported; malformed input is
// ClassTranslation. Errors are descriptive and redacted — no silent loss.
func BuildRequest(upstreamModel string, sourceFormat string, sourceBody []byte, tools ...*shared.ConversionContext) ([]byte, *errclass.Error) {
	switch sourceFormat {
	case "messages":
		return shared.RewriteModelID(upstreamModel, sourceBody, shared.FormatMessages)
	case "chat-completions":
		return fromChatCompletions(upstreamModel, sourceBody, shared.ContextOrNew(tools))
	case "responses":
		return fromResponses(upstreamModel, sourceBody, shared.ContextOrNew(tools))
	default:
		return nil, shared.UnsupportedFormat(sourceFormat, EndpointPath)
	}
}

func contentParts(raw json.RawMessage) ([]anthropicBlock, *errclass.Error) {
	return shared.ClaudeBlocksFromOpenAI(raw, EndpointPath)
}

// hasNonText keeps images and documents from being flattened into text.
func hasNonText(blocks []anthropicBlock) bool {
	for _, blk := range blocks {
		if blk["type"] != "text" {
			return true
		}
	}
	return false
}

// decodeStop accepts the OpenAI stop field as a string or array of strings.
func decodeStop(raw json.RawMessage) ([]string, *errclass.Error) {
	if !shared.HasContent(raw) {
		return nil, nil
	}
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		return []string{one}, nil
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err != nil {
		return nil, errclass.Translation("stop must be a string or an array of strings")
	}
	return many, nil
}

// systemField renders collected system text as a single string when one
// piece, or text blocks when several.
func systemField(parts []string) any {
	switch len(parts) {
	case 0:
		return nil
	case 1:
		return parts[0]
	default:
		blocks := make([]anthropicBlock, len(parts))
		for i, p := range parts {
			blocks[i] = textBlock(p)
		}
		return blocks
	}
}

// msgBuilder groups consecutive blocks into alternating-role Anthropic
// messages. Buckets with distinct keys merge only when both canonical
// roles are "user" (tool_result + later user text in one turn) — the
// Messages contract rejects consecutive same-role messages.
type msgBuilder struct {
	blocksAdded int
	curKey      string
	cur         *anthropicMessage
	msgs        []anthropicMessage
}

func (b *msgBuilder) add(key, role string, block anthropicBlock) {
	if b.cur == nil || (b.curKey != key && !(role == "user" && b.cur.Role == "user")) {
		b.flush()
		b.cur = &anthropicMessage{Role: role, Content: []anthropicBlock{}}
		b.curKey = key
	}
	b.cur.Content = append(b.cur.Content.([]anthropicBlock), block)
	b.blocksAdded++
}

func (b *msgBuilder) flush() {
	if b.cur == nil {
		return
	}
	blocks := b.cur.Content.([]anthropicBlock)
	if len(blocks) == 1 && blocks[0]["type"] == "text" && blocks[0]["cache_control"] == nil {
		b.cur.Content = blocks[0]["text"]
	}
	b.msgs = append(b.msgs, *b.cur)
	b.cur = nil
	b.curKey = ""
}

// finalize assembles the shared envelope fields, resolves the client
// reasoning effort to a fixed thinking budget without model validation.
// Levels with no numeric equivalent fail explicitly, and upstream validates
// the resulting controls.
func finalize(req *messagesRequest, system []string, effort string) ([]byte, *errclass.Error) {
	if req.System == nil {
		req.System = systemField(system)
	}
	if req.Messages == nil {
		req.Messages = []anthropicMessage{}
	}
	if effort != "" {
		budget, ok := thinking.BudgetFromEffort(effort)
		if !ok {
			return nil, &errclass.Error{Class: errclass.ClassUnsupported,
				Message: fmt.Sprintf("reasoning effort %q has no Messages budget equivalent", effort)}
		}
		applyThinking(req, budget)
	}
	b, _ := json.Marshal(req) // only marshallable composed types; cannot fail
	return b, nil
}

// applyThinking translates explicit off and fixed budgets. Auto leaves thinking
// unspecified so the upstream chooses its default; enabled requires a budget.
// Client output limits and sampling controls stay intact for upstream validation.
func applyThinking(req *messagesRequest, budget int64) {
	switch {
	case budget == 0:
		req.Thinking = &shared.ClaudeThinking{Type: "disabled"}
	case budget < 0:
		return
	default:
		req.Thinking = &shared.ClaudeThinking{Type: "enabled", BudgetTokens: &budget}
	}
}

// fromChatCompletions translates a Chat Completions request into a
// Messages request: leading system/developer messages
// become the top-level system field, assistant tool_calls become tool_use
// blocks, tool results become user tool_result blocks, stop becomes
// stop_sequences, tools become input_schema definitions, reasoning_effort
// becomes a fixed thinking budget.
//
// Explicit omission policy: fields with no Messages equivalent
// (logprobs, frequency_penalty, n, ...) are omitted by struct selection;
// everything representable is mapped or rejected descriptively.
func fromChatCompletions(upstreamModel string, body []byte, ctx ...*shared.ConversionContext) ([]byte, *errclass.Error) {
	var src shared.ChatCompletionsRequest
	if err := json.Unmarshal(body, &src); err != nil {
		return nil, errclass.Translation("malformed openai request JSON: " + err.Error())
	}
	// The shared kernel owns the defaulting policy: a positive
	// max_tokens (or max_completion_tokens fallback) passes through;
	// absent, zero, or negative defaults to 4096.
	maxTokens := int64(0)
	if src.MaxTokens != nil {
		maxTokens = *src.MaxTokens
	} else if src.MaxCompletionTokens != nil {
		maxTokens = *src.MaxCompletionTokens
	}
	req := &messagesRequest{
		Model:       upstreamModel,
		MaxTokens:   shared.ClaudeMaxTokens(maxTokens),
		Stream:      src.Stream,
		Temperature: src.Temperature,
		TopP:        src.TopP,
	}
	stop, eErr := decodeStop(src.Stop)
	if eErr != nil {
		return nil, eErr
	}
	req.StopSequences = stop
	allowUnsignedThinking := shared.ContextOrNew(ctx).Options.AllowUnsignedThinking

	var system []string
	var systemBlocks []anthropicBlock
	var b msgBuilder
	for i := range src.Messages {
		m := &src.Messages[i]
		start := b.blocksAdded
		switch m.Role {
		case "system", "developer":
			blocks, eErr := contentParts(m.Content)
			if eErr != nil {
				return nil, eErr
			}
			if hasNonText(blocks) {
				return nil, rejectNonText(blocks, true)
			}
			system = append(system, shared.JoinTexts(blocks))
			applyCacheControl(blocks, m.CacheControl)
			systemBlocks = append(systemBlocks, blocks...)
		case "assistant":
			if text := m.ReasoningFields.ReplayText(); text != "" && allowUnsignedThinking {
				b.add("assistant", "assistant", anthropicBlock{"type": "thinking", "thinking": text})
			}
			blocks, eErr := contentParts(m.Content)
			if eErr != nil {
				return nil, eErr
			}
			if hasNonText(blocks) {
				return nil, rejectNonText(blocks, false)
			}
			for _, block := range assistantTextBlocks(blocks) {
				b.add("assistant", "assistant", block)
			}
			for _, tc := range m.ToolCalls {
				input, eErr := shared.DecodeArgs(tc.Function.Arguments)
				if eErr != nil {
					return nil, eErr
				}
				b.add("assistant", "assistant", anthropicBlock{
					"type": "tool_use", "id": tc.ID, "name": tc.Function.Name, "input": input,
				})
			}
		case "tool":
			blocks, eErr := contentParts(m.Content)
			if eErr != nil {
				return nil, eErr
			}
			block, eErr := toolResultBlock(m.ToolCallID, m.Content, blocks)
			if eErr != nil {
				return nil, eErr
			}
			b.add("user:tool", "user", block)
		case "user":
			blocks, eErr := contentParts(m.Content)
			if eErr != nil {
				return nil, eErr
			}
			for _, blk := range blocks { // part order is preserved
				b.add("user", "user", blk)
			}
		default:
			return nil, shared.ValidateRole(m.Role, EndpointPath)
		}
		b.applyCacheSince(start, m.CacheControl)
	}
	b.flush()
	req.Messages = b.msgs
	if hasCacheControl(systemBlocks) {
		req.System = systemBlocks
	}

	for _, t := range src.Tools {
		if t.Type == "namespace" {
			continue
		}
		if eErr := shared.FunctionTool(t.Type, EndpointPath); eErr != nil {
			return nil, eErr
		}
		req.Tools = append(req.Tools, anthropicTool{
			Name: t.Function.Name, Description: t.Function.Description,
			InputSchema:  shared.ObjectSchema(t.Function.Parameters),
			Strict:       t.Function.Strict,
			CacheControl: toolCacheControl(t),
		})
	}
	kind, tcName, eErr := shared.DecodeToolChoice(src.ToolChoice)
	if eErr != nil {
		return nil, eErr
	}
	if eErr := applyToolChoiceMessages(req, kind, tcName, src.ParallelToolCalls); eErr != nil {
		return nil, eErr
	}
	format, err := shared.ConvertOutputFormat("chat-completions", "messages", src.ResponseFormat, shared.ContextOrNew(ctx))
	if err != nil {
		return nil, err
	}
	req.OutputConfig = format
	return finalize(req, system, src.ReasoningEffort)
}

// fromResponses translates an OpenAI Responses request into a Messages
// request: instructions become the system field, message items
// map like Chat Completions messages, function_call/function_call_output
// items map to tool_use/tool_result blocks. Original reasoning content becomes
// unsigned thinking only when explicitly enabled for a compatible upstream.
func fromResponses(upstreamModel string, body []byte, tools *shared.ConversionContext) ([]byte, *errclass.Error) {
	var src shared.ResponsesRequest
	if err := json.Unmarshal(body, &src); err != nil {
		return nil, errclass.Translation("malformed Responses request JSON: " + err.Error())
	}
	// Same shared kernel policy as the Chat Completions leg.
	maxTokens := int64(0)
	if src.MaxOutputTokens != nil {
		maxTokens = *src.MaxOutputTokens
	}
	req := &messagesRequest{
		Model:       upstreamModel,
		MaxTokens:   shared.ClaudeMaxTokens(maxTokens),
		Stream:      src.Stream,
		Temperature: src.Temperature,
		TopP:        src.TopP,
	}

	var system []string
	var systemBlocks []anthropicBlock
	instr, eErr := src.DecodeInstructions()
	if eErr != nil {
		return nil, eErr
	}
	if instr != "" {
		system = append(system, instr)
		systemBlocks = append(systemBlocks, textBlock(instr))
	}

	var b msgBuilder
	items, eErr := tools.Normalize(&src, EndpointPath)
	if eErr != nil {
		return nil, eErr
	}
	for _, item := range items {
		start := b.blocksAdded
		switch item.Type {
		case "message":
			blocks, eErr := contentParts(item.Content)
			if eErr != nil {
				return nil, eErr
			}
			switch item.Role {
			case "user":
				for _, blk := range blocks { // part order is preserved
					b.add("user", "user", blk)
				}
			case "assistant":
				if hasNonText(blocks) {
					return nil, rejectNonText(blocks, false)
				}
				for _, block := range assistantTextBlocks(blocks) {
					b.add("assistant", "assistant", block)
				}
			case "system", "developer":
				if hasNonText(blocks) {
					return nil, rejectNonText(blocks, true)
				}
				system = append(system, shared.JoinTexts(blocks))
				applyCacheControl(blocks, item.CacheControl)
				systemBlocks = append(systemBlocks, blocks...)
			default:
				return nil, shared.ValidateRole(item.Role, EndpointPath)
			}
		case "function_call":
			input, eErr := shared.DecodeArgs(item.Arguments)
			if eErr != nil {
				return nil, eErr
			}
			b.add("assistant", "assistant", anthropicBlock{
				"type": "tool_use", "id": item.CallID, "name": item.Name, "input": input,
			})
		case "custom_tool_call":
			args := item.Arguments
			if args == "" && item.Input != "" {
				args = item.Input
			}
			input, eErr := shared.DecodeArgs(args)
			if eErr != nil {
				// Freeform custom tool input is not JSON: wrap verbatim.
				input = map[string]any{"input": args}
			}
			b.add("assistant", "assistant", anthropicBlock{
				"type": "tool_use", "id": item.CallID, "name": item.Name, "input": input,
			})
		case "function_call_output", "custom_tool_call_output":
			blocks, eErr := contentParts(item.Output)
			if eErr != nil {
				return nil, eErr
			}
			block, eErr := toolResultBlock(item.CallID, item.Output, blocks)
			if eErr != nil {
				return nil, eErr
			}
			b.add("user:tool", "user", block)
		case "reasoning":
			// A summary cannot stand in for original reasoning. Signatures
			// cannot be manufactured; the public loss policy reports this.
			if text := shared.ReasoningContent(item.Content); text != "" && tools.Options.AllowUnsignedThinking {
				b.add("assistant", "assistant", anthropicBlock{
					"type": "thinking", "thinking": text,
				})
			}
		default:
			return nil, shared.UnsupportedInputItemType(item.Type)
		}
		b.applyCacheSince(start, item.CacheControl)
	}
	b.flush()
	req.Messages = b.msgs
	if hasCacheControl(systemBlocks) {
		req.System = systemBlocks
	}

	for _, t := range src.Tools {
		if shared.IsSearchTool(t.Type) {
			mapped := shared.ResponseSearchTool(t)
			req.Tools = append(req.Tools, anthropicTool{Type: mapped.Type, Name: mapped.Name, UserLocation: mapped.UserLocation, AllowedDomains: mapped.AllowedDomains, CacheControl: t.CacheControl})
			continue
		}
		if eErr := shared.FunctionTool(t.Type, EndpointPath); eErr != nil {
			return nil, eErr
		}
		schema := shared.ObjectSchema(t.Parameters)
		req.Tools = append(req.Tools, anthropicTool{
			Name: t.Name, Description: t.Description, InputSchema: schema, Strict: t.Strict, CacheControl: t.CacheControl,
		})
	}
	effort := ""
	if src.Reasoning != nil {
		effort = src.Reasoning.Effort
	}
	choice, choiceErr := shared.HostedChoice(src.ToolChoice, "messages")
	if choiceErr != nil {
		return nil, choiceErr
	}
	kind, tcName, eErr := shared.DecodeToolChoice(choice)
	if eErr != nil {
		return nil, eErr
	}
	if eErr := applyToolChoiceMessages(req, kind, tcName, src.ParallelToolCalls); eErr != nil {
		return nil, eErr
	}
	format, err := shared.ConvertOutputFormat("responses", "messages", src.Text, tools)
	if err != nil {
		return nil, err
	}
	req.OutputConfig = format
	return finalize(req, system, effort)
}

// applyToolChoiceMessages maps a normalized tool choice onto the outbound
// Messages request. "none" has no Anthropic equivalent and fails
// descriptively rather than silently unforcing the client constraint
// (explicit policy). An explicit parallel_tool_calls=false is a
// client constraint too: it becomes disable_parallel_tool_use on
// the synthesized or decoded tool_choice — synthesized as {"type":"auto"}
// when no tool_choice was sent, so the flag never rides alone.
func applyToolChoiceMessages(req *messagesRequest, kind, name string, parallel *bool) *errclass.Error {
	noParallel := parallel != nil && !*parallel
	if kind == shared.ToolChoiceAbsent && noParallel {
		kind = shared.ToolChoiceAuto
	}
	switch kind {
	case shared.ToolChoiceAbsent:
		return nil
	case shared.ToolChoiceAuto:
		req.ToolChoice = toolChoiceBlock("auto", "", noParallel)
	case shared.ToolChoiceAny:
		req.ToolChoice = toolChoiceBlock("any", "", noParallel)
	case shared.ToolChoiceNamed:
		req.ToolChoice = toolChoiceBlock("tool", name, noParallel)
	default:
		return &errclass.Error{
			Class:   errclass.ClassUnsupported,
			Message: fmt.Sprintf("tool_choice %q cannot be represented for %s", kind, EndpointPath),
		}
	}
	return nil
}

// toolChoiceBlock renders one outbound tool_choice object; noParallel is
// Anthropic's disable_parallel_tool_use flag.
func toolChoiceBlock(typ, name string, noParallel bool) map[string]any {
	tc := map[string]any{"type": typ}
	if name != "" {
		tc["name"] = name
	}
	if noParallel {
		tc["disable_parallel_tool_use"] = true
	}
	return tc
}

func rejectNonText(blocks []anthropicBlock, system bool) *errclass.Error {
	for _, block := range blocks {
		if block["type"] == "document" {
			return shared.UnsupportedPartType("document", "system or assistant message")
		}
	}
	if system {
		return shared.SystemImageRejected()
	}
	return errclass.Translation("assistant messages cannot carry image content")
}

// toolResultContent preserves text/image order and rejects document media.
func toolResultContent(blocks []anthropicBlock) (any, *errclass.Error) {
	images := false
	for _, block := range blocks {
		switch typ, _ := block["type"].(string); typ {
		case "text":
		case "image":
			images = true
		default:
			return nil, shared.UnsupportedPartType(typ, "Messages tool_result.content")
		}
	}
	if images {
		return blocks, nil
	}
	return shared.JoinTexts(blocks), nil
}
