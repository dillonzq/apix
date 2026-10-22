package shared

import (
	"encoding/json"
	"strings"
)

// outputTextPart is one content part of a synthesized assistant message
// item.
type outputTextPart struct {
	Type        string           `json:"type"` // always "output_text"
	Text        string           `json:"text"`
	Annotations []map[string]any `json:"annotations,omitempty"`
}

// OutputAssembler aggregates Responses output items — ONE assistant
// message item carrying all observed text plus function_call items in
// arrival order — for every Responses-output synthesis site, streaming and
// non-streaming, Messages-source and Chat-Completions-source alike
// (mode/route parity). A site that called ReserveTextSlot gets the
// message at the first text observation's slot (filled even when the
// aggregated text is empty — Messages-source semantics); a site that
// never reserves gets the Chat Completions placement (message leads, and
// only when text is non-empty). Render materializes the array once, at
// terminal time.
type OutputAssembler struct {
	messageID    string // identity carried by the message item
	items        []any  // rendered items in insertion order
	textSlot     int    // reserved message position, -1 until reserved
	refusal      strings.Builder
	refusalFirst bool
	text         strings.Builder    // aggregated message text
	tools        *ConversionContext // wire-to-original tool identities for restoration
	annotations  []map[string]any
}

// NewOutputAssembler binds an assembler to the response identity the
// synthesized message item carries.
func NewOutputAssembler(messageID string, tools ...*ConversionContext) *OutputAssembler {
	a := &OutputAssembler{messageID: messageID, textSlot: -1, items: make([]any, 0)}
	if len(tools) > 0 && tools[0] != nil {
		a.tools = tools[0]
	}
	return a
}

// ReserveTextSlot pins the message item's position at the current end of
// the output array so later function_call items cannot displace it;
// idempotent — only the first call reserves.
func (a *OutputAssembler) ReserveTextSlot() {
	if a.textSlot >= 0 {
		return
	}
	a.textSlot = len(a.items)
	a.items = append(a.items, nil)
}

// AddText appends one text fragment to the aggregated message.
func (a *OutputAssembler) AddText(fragment string) {
	a.text.WriteString(fragment)
}

// AppendFunctionCall adds one function_call item in arrival order;
// arguments pass through verbatim — callers apply the absent-arguments
// policy themselves. Tools originally declared as type "custom" restore
// as custom_tool_call items with unwrapped input.
func (a *OutputAssembler) AppendFunctionCall(callID, name, args string) {
	id := a.tools.Identity(name)
	if id.isShell {
		action, err := ShellAction(args)
		if err == nil {
			a.items = append(a.items, RespItem{Type: "shell_call", ID: ShellItemID(callID), CallID: callID, Action: shellResponseAction(action), Environment: json.RawMessage(shellEnvironment), Status: "completed"})
			return
		}
	}
	if id.isCustom {
		a.items = append(a.items, RespItem{
			Type: "custom_tool_call", CallID: callID, Name: id.name, Namespace: id.namespace, Input: UnwrapCustomToolInput(args),
		})
		return
	}
	a.items = append(a.items, RespItem{
		Type: "function_call", CallID: callID, Name: id.name, Namespace: id.namespace, Arguments: args,
	})
}

// Render fills the reserved slot (or leads with the message for sites
// that never reserved but observed non-empty text) and returns the
// output array.
func (a *OutputAssembler) Render() []any {
	t := a.text.String()
	refusal := a.refusal.String()
	if t != "" || refusal != "" || a.textSlot >= 0 {
		parts := make([]any, 0, 2)
		if t != "" || refusal == "" {
			parts = append(parts, outputTextPart{Type: "output_text", Text: t, Annotations: a.annotations})
		}
		if refusal != "" {
			part := struct {
				Type    string `json:"type"`
				Refusal string `json:"refusal"`
			}{Type: "refusal", Refusal: refusal}
			if a.refusalFirst {
				parts = append([]any{part}, parts...)
			} else {
				parts = append(parts, part)
			}
		}
		content, _ := json.Marshal(parts)
		msg := RespItem{Type: "message", ID: a.messageID, Role: "assistant", Content: content}
		if a.textSlot >= 0 {
			a.items[a.textSlot] = msg
		} else {
			rest := a.items
			a.items = make([]any, 0, len(rest)+1)
			a.items = append(a.items, msg)
			a.items = append(a.items, rest...)
		}
	}
	return a.items
}

func (a *OutputAssembler) AddCitations(citations []Citation) {
	a.annotations = append(a.annotations, CitationValues(citations, FormatResponses)...)
}

func (a *OutputAssembler) AppendItem(item any) { a.items = append(a.items, item) }
