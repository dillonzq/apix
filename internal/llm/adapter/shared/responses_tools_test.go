package shared

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

func reqWithInput(t *testing.T, input any, tools []RespTool, toolChoice any) *ResponsesRequest {
	t.Helper()
	r := &ResponsesRequest{Tools: tools}
	if input != nil {
		b, err := json.Marshal(input)
		if err != nil {
			t.Fatalf("marshal input: %v", err)
		}
		r.Input = b
	}
	if toolChoice != nil {
		switch tc := toolChoice.(type) {
		case string:
			r.ToolChoice = json.RawMessage(tc)
		default:
			b, err := json.Marshal(tc)
			if err != nil {
				t.Fatalf("marshal tool_choice: %v", err)
			}
			r.ToolChoice = b
		}
	}
	return r
}

func TestIdentity(t *testing.T) {
	var nilTools *ConversionContext
	if got := nilTools.Identity("fn"); got != (responseToolIdentity{name: "fn"}) {
		t.Fatalf("nil registry = %+v", got)
	}
	rt := NewConversionContext()
	if got := rt.Identity("unknown"); got != (responseToolIdentity{name: "unknown"}) {
		t.Fatalf("unknown = %+v", got)
	}
	rt.Tools.names["ns__fn"] = responseToolIdentity{name: "fn", namespace: "ns"}
	if got := rt.Identity("ns__fn"); got != (responseToolIdentity{name: "fn", namespace: "ns"}) {
		t.Fatalf("known = %+v", got)
	}
}

func TestNormalizeMergesAdditionalTools(t *testing.T) {
	r := reqWithInput(t, []any{
		map[string]any{"type": "additional_tools", "tools": []any{
			map[string]any{"type": "function", "name": "extra_search"},
		}},
		map[string]any{"type": "message", "role": "user", "content": "hi"},
	}, []RespTool{{Type: "function", Name: "base"}}, nil)
	conv, eErr := NewConversionContext().Normalize(r, "/v1/responses")
	if eErr != nil {
		t.Fatalf("Normalize: %v", eErr)
	}
	if len(conv) != 1 || conv[0].Type != "message" {
		t.Fatalf("conv = %+v", conv)
	}
	if len(r.Tools) != 2 || r.Tools[0].Name != "base" || r.Tools[1].Name != "extra_search" {
		t.Fatalf("tools = %+v", r.Tools)
	}
}

func TestNormalizeAdditionalToolsErrors(t *testing.T) {
	for name, item := range map[string]any{
		"missing": map[string]any{"type": "additional_tools"},
		"null":    map[string]any{"type": "additional_tools", "tools": nil},
	} {
		r := reqWithInput(t, []any{item}, nil, nil)
		if _, eErr := NewConversionContext().Normalize(r, "/v1/responses"); eErr == nil ||
			!strings.Contains(eErr.Message, "additional_tools requires a tools array") {
			t.Fatalf("%s: err = %v", name, eErr)
		}
	}
	// Empty (non-nil) array merges zero tools without error.
	r := reqWithInput(t, []any{map[string]any{"type": "additional_tools", "tools": []any{}}}, nil, nil)
	if _, eErr := NewConversionContext().Normalize(r, "/v1/responses"); eErr != nil {
		t.Fatalf("empty array: %v", eErr)
	}
}

func TestNormalizeDecodeInputError(t *testing.T) {
	r := &ResponsesRequest{Input: json.RawMessage(`[bad`)}
	if _, eErr := NewConversionContext().Normalize(r, "/v1/responses"); eErr == nil {
		t.Fatal("want decode error")
	}
}

func TestNormalizeUnrollsNamespace(t *testing.T) {
	r := reqWithInput(t, "hello", []RespTool{{
		Type: "namespace", Name: "subagents",
		Tools: []RespTool{{Type: "function", Name: "spawn_agent"}},
	}}, nil)
	conv, eErr := NewConversionContext().Normalize(r, "/v1/responses")
	if eErr != nil {
		t.Fatalf("Normalize: %v", eErr)
	}
	if len(conv) != 1 || conv[0].Type != "message" {
		t.Fatalf("string input conv = %+v", conv)
	}
	if len(r.Tools) != 1 || r.Tools[0].Name != "subagents__spawn_agent" {
		t.Fatalf("tools = %+v", r.Tools)
	}
}

func TestNormalizeNamespaceErrors(t *testing.T) {
	for name, tool := range map[string]RespTool{
		"empty name":   {Type: "namespace", Name: "  ", Tools: []RespTool{{Type: "function", Name: "f"}}},
		"nil tools":    {Type: "namespace", Name: "ns"},
		"non-function": {Type: "namespace", Name: "ns", Tools: []RespTool{{Type: "web_search", Name: "f"}}},
	} {
		r := &ResponsesRequest{Tools: []RespTool{tool}}
		if _, eErr := NewConversionContext().Normalize(r, "/v1/responses"); eErr == nil {
			t.Fatalf("%s: want error", name)
		}
	}
	// Empty (non-nil) namespace children: vacuous, no error.
	r := &ResponsesRequest{Tools: []RespTool{{Type: "namespace", Name: "subagents", Tools: []RespTool{}}}}
	if _, eErr := NewConversionContext().Normalize(r, "/v1/responses"); eErr != nil {
		t.Fatalf("empty children: %v", eErr)
	}
}

// Plain top-level tools pass through untouched: namespace validation
// applies only to namespace children; adapters validate the rest.
func TestNormalizePlainToolsPassthrough(t *testing.T) {
	r := &ResponsesRequest{Tools: []RespTool{
		{Type: "function", Name: "f"},
		{Type: "web_search", Name: "w"},
	}}
	if _, eErr := NewConversionContext().Normalize(r, "/v1/responses"); eErr != nil {
		t.Fatalf("Normalize: %v", eErr)
	}
	if len(r.Tools) != 2 {
		t.Fatalf("tools = %+v", r.Tools)
	}
}

func TestNormalizeTruncation(t *testing.T) {
	ns := strings.Repeat("n", 40)
	fn := strings.Repeat("f", 40)
	wantFull := ns + "__" + fn
	if len(wantFull) <= 64 {
		t.Fatalf("fixture too short: %d", len(wantFull))
	}
	r := reqWithInput(t, nil, []RespTool{{
		Type: "namespace", Name: ns, Tools: []RespTool{{Type: "function", Name: fn}},
	}}, nil)
	if _, eErr := NewConversionContext().Normalize(r, "/v1/responses"); eErr != nil {
		t.Fatalf("Normalize: %v", eErr)
	}
	sum := sha256.Sum256([]byte(wantFull))
	want := wantFull[:50] + "__" + hex.EncodeToString(sum[:6])
	if len(want) != 64 {
		t.Fatalf("want len 64, got %d", len(want))
	}
	if r.Tools[0].Name != want {
		t.Fatalf("got %q want %q", r.Tools[0].Name, want)
	}
	// Exactly 64 chars: no truncation.
	exact := strings.Repeat("a", 30) + "__" + strings.Repeat("b", 32)
	if len(exact) != 64 {
		t.Fatalf("fixture len %d", len(exact))
	}
	if got := qualifiedToolName(strings.Repeat("b", 32), strings.Repeat("a", 30)); got != exact {
		t.Fatalf("boundary truncated: %q", got)
	}
}

func TestNormalizeCollision(t *testing.T) {
	r := &ResponsesRequest{Tools: []RespTool{
		{Type: "namespace", Name: "x", Tools: []RespTool{{Type: "function", Name: "y__z"}}},
		{Type: "namespace", Name: "x__y", Tools: []RespTool{{Type: "function", Name: "z"}}},
	}}
	if _, eErr := NewConversionContext().Normalize(r, "/v1/responses"); eErr == nil ||
		!strings.Contains(eErr.Message, "collide after namespace conversion") {
		t.Fatalf("err = %v", eErr)
	}
}

func TestNormalizeDuplicateIdenticalFirstWins(t *testing.T) {
	r := &ResponsesRequest{Tools: []RespTool{
		{Type: "namespace", Name: "ns", Tools: []RespTool{
			{Type: "function", Name: "f", Description: "first"},
		}},
		{Type: "namespace", Name: "ns", Tools: []RespTool{
			{Type: "function", Name: "f", Description: "second"},
		}},
	}}
	rt := NewConversionContext()
	if _, eErr := rt.Normalize(r, "/v1/responses"); eErr != nil {
		t.Fatalf("Normalize: %v", eErr)
	}
	if len(r.Tools) != 1 || r.Tools[0].Description != "first" {
		t.Fatalf("tools = %+v", r.Tools)
	}
}

func TestNormalizeRewritesFunctionCallHistory(t *testing.T) {
	r := reqWithInput(t, []any{
		map[string]any{"type": "function_call", "call_id": "c1", "name": "spawn_agent", "namespace": "subagents", "arguments": "{}"},
		map[string]any{"type": "function_call", "call_id": "c2", "name": "plain", "arguments": "{}"},
	}, nil, nil)
	rt := NewConversionContext()
	conv, eErr := rt.Normalize(r, "/v1/responses")
	if eErr != nil {
		t.Fatalf("Normalize: %v", eErr)
	}
	if conv[0].Name != "subagents__spawn_agent" || conv[0].Namespace != "" {
		t.Fatalf("rewritten = %+v", conv[0])
	}
	if conv[1].Name != "plain" {
		t.Fatalf("plain touched = %+v", conv[1])
	}
	if got := rt.Identity("subagents__spawn_agent"); got != (responseToolIdentity{name: "spawn_agent", namespace: "subagents"}) {
		t.Fatalf("registry = %+v", got)
	}
	// Pre-registered identity: rewrite reuses the existing entry.
	r2 := reqWithInput(t, []any{
		map[string]any{"type": "function_call", "call_id": "c3", "name": "spawn_agent", "namespace": "subagents", "arguments": "{}"},
	}, []RespTool{{Type: "namespace", Name: "subagents", Tools: []RespTool{{Type: "function", Name: "spawn_agent"}}}},
		nil)
	rt2 := NewConversionContext()
	if _, eErr := rt2.Normalize(r2, "/v1/responses"); eErr != nil {
		t.Fatalf("Normalize: %v", eErr)
	}
}

func TestNormalizeRewritesToolChoice(t *testing.T) {
	r := reqWithInput(t, "go", []RespTool{{
		Type: "namespace", Name: "subagents", Tools: []RespTool{{Type: "function", Name: "spawn_agent"}},
	}}, map[string]any{"type": "function", "name": "spawn_agent", "namespace": "subagents"})
	rt := NewConversionContext()
	if _, eErr := rt.Normalize(r, "/v1/responses"); eErr != nil {
		t.Fatalf("Normalize: %v", eErr)
	}
	var tc struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(r.ToolChoice, &tc); err != nil {
		t.Fatalf("tool_choice not JSON: %v", err)
	}
	if tc.Name != "subagents__spawn_agent" {
		t.Fatalf("tool_choice = %+v", tc)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(r.ToolChoice, &raw); err != nil {
		t.Fatalf("tool_choice raw: %v", err)
	}
	if _, ok := raw["namespace"]; ok {
		t.Fatalf("namespace not stripped: %s", r.ToolChoice)
	}
}

func TestNormalizeToolChoicePassthrough(t *testing.T) {
	// No tool_choice.
	r := reqWithInput(t, nil, nil, nil)
	if _, eErr := (&ConversionContext{}).Normalize(r, "/v1/responses"); eErr != nil {
		t.Fatalf("nil-map init: %v", eErr)
	}
	// String choice, object without namespace, malformed JSON: untouched.
	for name, tc := range map[string]string{
		"string":    `"auto"`,
		"no ns":     `{"type":"function","name":"f"}`,
		"malformed": `{bad`,
	} {
		r := &ResponsesRequest{ToolChoice: json.RawMessage(tc)}
		if _, eErr := NewConversionContext().Normalize(r, "/v1/responses"); eErr != nil {
			t.Fatalf("%s: %v", name, eErr)
		}
		if string(r.ToolChoice) != tc {
			t.Fatalf("%s: rewritten to %s", name, r.ToolChoice)
		}
	}
	// Structured choice without a "type" still flattens the namespaced name.
	r = &ResponsesRequest{ToolChoice: json.RawMessage(`{"name":"f","namespace":"ns"}`)}
	if _, eErr := NewConversionContext().Normalize(r, "/v1/responses"); eErr != nil {
		t.Fatalf("typeless choice: %v", eErr)
	}
	var tc2 struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(r.ToolChoice, &tc2); err != nil || tc2.Name != "ns__f" {
		t.Fatalf("typeless rewritten = %s (%v)", r.ToolChoice, err)
	}
	// Undeclared namespaced choice registers on the fly.
	r2 := &ResponsesRequest{ToolChoice: json.RawMessage(`{"type":"function","name":"f","namespace":"ns"}`)}
	rt := NewConversionContext()
	if _, eErr := rt.Normalize(r2, "/v1/responses"); eErr != nil {
		t.Fatalf("undeclared choice: %v", eErr)
	}
	if got := rt.Identity("ns__f"); got != (responseToolIdentity{name: "f", namespace: "ns"}) {
		t.Fatalf("registry = %+v", got)
	}
}

func TestNormalizeCustomToolBecomesFunction(t *testing.T) {
	r := &ResponsesRequest{Tools: []RespTool{{Type: "custom", Name: "exec"}}}
	if _, eErr := NewConversionContext().Normalize(r, "/v1/chat/completions"); eErr != nil {
		t.Fatalf("Normalize: %v", eErr)
	}
	if len(r.Tools) != 1 {
		t.Fatalf("tools = %+v", r.Tools)
	}
	tool := r.Tools[0]
	if tool.Type != "function" {
		t.Fatalf("type = %q want %q", tool.Type, "function")
	}
	if tool.Name != "exec" {
		t.Fatalf("name = %q want %q", tool.Name, "exec")
	}
	var params map[string]any
	if err := json.Unmarshal(tool.Parameters, &params); err != nil {
		t.Fatalf("parameters unmarshal %s: %v", string(tool.Parameters), err)
	}
	if params["type"] != "object" {
		t.Fatalf("parameters.type = %+v", params)
	}
	props, ok := params["properties"].(map[string]any)
	if !ok || len(props) != 1 || props["input"].(map[string]any)["type"] != "string" || params["additionalProperties"] != false {
		t.Fatalf("parameters.properties = %+v", params)
	}
}

func TestNormalizeWebSearchDropped(t *testing.T) {
	r := &ResponsesRequest{Tools: []RespTool{
		{Type: "web_search"},
		{Type: "web_search_preview"},
		{Type: "function", Name: "f1"},
	}}
	if _, eErr := NewConversionContext().Normalize(r, "/v1/chat/completions"); eErr != nil {
		t.Fatalf("Normalize: %v", eErr)
	}
	if len(r.Tools) != 1 || r.Tools[0].Name != "f1" {
		t.Fatalf("tools = %+v", r.Tools)
	}
	// Messages keeps hosted search declarations for its native search mapping.
	r = &ResponsesRequest{Tools: []RespTool{
		{Type: "web_search"},
		{Type: "web_search_preview"},
		{Type: "function", Name: "f1"},
	}}
	if _, eErr := NewConversionContext().Normalize(r, "/v1/messages"); eErr != nil {
		t.Fatalf("Normalize: %v", eErr)
	}
	if len(r.Tools) != 3 || r.Tools[2].Name != "f1" {
		t.Fatalf("tools = %+v", r.Tools)
	}
	r = &ResponsesRequest{Tools: []RespTool{
		{Type: "web_search"},
		{Type: "web_search_preview"},
		{Type: "function", Name: "f1"},
	}}
	if _, eErr := NewConversionContext().Normalize(r, "/v1/responses"); eErr != nil {
		t.Fatalf("Normalize: %v", eErr)
	}
	if len(r.Tools) != 3 {
		t.Fatalf("tools = %+v", r.Tools)
	}
	// Namespace children drop identically on translated routes.
	r = &ResponsesRequest{Tools: []RespTool{{
		Type: "namespace", Name: "ns",
		Tools: []RespTool{{Type: "web_search"}, {Type: "function", Name: "f1"}},
	}}}
	if _, eErr := NewConversionContext().Normalize(r, "/v1/chat/completions"); eErr != nil {
		t.Fatalf("Normalize: %v", eErr)
	}
	if len(r.Tools) != 1 || r.Tools[0].Name != "ns__f1" {
		t.Fatalf("tools = %+v", r.Tools)
	}
}

func TestNormalizeApplyPatchPreserved(t *testing.T) {
	rt := NewConversionContext()
	r := &ResponsesRequest{Tools: []RespTool{{Type: "custom", Name: "apply_patch"}}}
	if _, err := rt.Normalize(r, "/v1/chat/completions"); err != nil {
		t.Fatal(err)
	}
	if len(r.Tools) != 1 || r.Tools[0].Name != "apply_patch" || r.Tools[0].Type != "function" || !rt.IsCustom("apply_patch") {
		t.Fatalf("custom patch identity lost: %+v", r.Tools)
	}
}

func TestOutputAssemblerCustomToolCallEmission(t *testing.T) {
	rt := NewConversionContext()
	r := &ResponsesRequest{Tools: []RespTool{{Type: "custom", Name: "exec"}}}
	if _, eErr := rt.Normalize(r, "/v1/chat/completions"); eErr != nil {
		t.Fatalf("Normalize: %v", eErr)
	}
	oa := NewOutputAssembler("msg_1", rt)
	oa.AppendFunctionCall("call_1", "exec", "{\"input\":\"console.log('hi')\"}")
	res := oa.Render()
	if len(res) != 1 {
		t.Fatalf("output len = %d want 1 (%v)", len(res), res)
	}
	item, ok := res[0].(RespItem)
	if !ok {
		t.Fatalf("output[0] type = %T want RespItem", res[0])
	}
	if item.Type != "custom_tool_call" {
		t.Fatalf("type = %q want %q (full: %+v)", item.Type, "custom_tool_call", item)
	}
	if item.CallID != "call_1" {
		t.Fatalf("call_id = %q want %q", item.CallID, "call_1")
	}
	if item.Name != "exec" {
		t.Fatalf("name = %q want %q", item.Name, "exec")
	}
	if item.Input != "console.log('hi')" {
		t.Fatalf("input = %q want %q (full: %+v)", item.Input, "console.log('hi')", item)
	}
	if item.Arguments != "" {
		t.Fatalf("arguments must be empty for custom_tool_call, got %q", item.Arguments)
	}
}

func TestNamespacedCustomToolCompletionEmitsInputDone(t *testing.T) {
	rt := NewConversionContext()
	r := &ResponsesRequest{Tools: []RespTool{{
		Type: "namespace", Name: "subagents",
		Tools: []RespTool{{Type: "custom", Name: "custom_sub"}},
	}}}
	if _, eErr := rt.Normalize(r, "/v1/chat/completions"); eErr != nil {
		t.Fatalf("Normalize: %v", eErr)
	}
	qualified := r.Tools[0].Name
	if !rt.IsCustom(qualified) {
		t.Fatalf("qualified %q must be custom", qualified)
	}
	em := ResponsesEventEmitter{ID: "resp_1", Model: "m", Tools: rt}
	raw := em.InputDone("call_1", 0, "hello")
	s := string(raw)
	if !strings.HasPrefix(s, "event: response.custom_tool_call_input.done\n") {
		t.Fatalf("event name wrong: %q", s)
	}
	_, data, ok := strings.Cut(s, "\ndata: ")
	if !ok {
		t.Fatalf("missing data line: %q", s)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSuffix(data, "\n\n")), &payload); err != nil {
		t.Fatalf("payload not JSON: %v (%q)", err, data)
	}
	if payload["type"] != "response.custom_tool_call_input.done" {
		t.Fatalf("type = %v want response.custom_tool_call_input.done (full: %v)", payload["type"], payload)
	}
	if payload["type"] == "response.function_call_arguments.done" {
		t.Fatalf("must not emit function_call_arguments.done for custom tool: %v", payload)
	}
	if payload["item_id"] != "call_1" || payload["output_index"] != float64(0) || payload["input"] != "hello" {
		t.Fatalf("payload wrong: %v", payload)
	}
}

func TestNormalizeNamespaceCustomChildUnrolled(t *testing.T) {
	r := &ResponsesRequest{Tools: []RespTool{{
		Type: "namespace", Name: "subagents",
		Tools: []RespTool{{Type: "custom", Name: "custom_sub"}},
	}}}
	if _, eErr := NewConversionContext().Normalize(r, "/v1/chat/completions"); eErr != nil {
		t.Fatalf("Normalize: %v", eErr)
	}
	if len(r.Tools) != 1 {
		t.Fatalf("tools = %+v", r.Tools)
	}
	if r.Tools[0].Type != "function" {
		t.Fatalf("type = %q want %q", r.Tools[0].Type, "function")
	}
	if r.Tools[0].Name != "subagents__custom_sub" {
		t.Fatalf("name = %q want %q", r.Tools[0].Name, "subagents__custom_sub")
	}
}
