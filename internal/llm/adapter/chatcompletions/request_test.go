package chatcompletions

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/dillonzq/apix/internal/errclass"
	"github.com/dillonzq/apix/internal/llm/adapter/shared"
)

func decodeOut(t *testing.T, out []byte, eErr *errclass.Error) map[string]any {
	t.Helper()
	if eErr != nil {
		t.Fatalf("unexpected error: %v", eErr)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	return m
}

func mustBuild(t *testing.T, sourceFormat, body string) map[string]any {
	t.Helper()
	out, eErr := BuildRequest("m", sourceFormat, []byte(body))
	return decodeOut(t, out, eErr)
}

func TestBuildRequestUnsupportedFormat(t *testing.T) {
	_, eErr := BuildRequest("m", "grpc", []byte(`{}`))
	if eErr == nil || eErr.Class != errclass.ClassUnsupported {
		t.Fatalf("want ClassUnsupported, got %+v", eErr)
	}
}

func TestBuildRequestOpenAI(t *testing.T) {
	body := `{"model":"pub/glm","messages":[{"role":"user","content":"hi"}],"stream":true,"max_tokens":32}`
	m := mustBuild(t, "chat-completions", body)
	if m["model"] != "m" || m["stream"] != true || m["max_tokens"] != float64(32) {
		t.Fatalf("passthrough broken: %v", m)
	}
	msgs := m["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages changed: %v", msgs)
	}
	if _, eErr := BuildRequest("m", "chat-completions", []byte(`{`)); eErr == nil || eErr.Class != errclass.ClassTranslation {
		t.Fatalf("want ClassTranslation, got %+v", eErr)
	}
}

func TestBuildRequestThinkingEffort(t *testing.T) {
	cases := []struct {
		name   string
		budget int64
		want   string
	}{
		{"small budget anchors low", 1024, "low"},
		{"mid budget anchors medium", 8192, "medium"},
		{"huge budget maps to xhigh", 131072, "xhigh"},
		{"large budget maps to xhigh", 60000, "xhigh"},
		{"zero budget disables thinking", 0, "none"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := fmt.Sprintf(
				`{"model":"x","max_tokens":64,"thinking":{"type":"enabled","budget_tokens":%d}}`,
				tc.budget)
			out, eErr := BuildRequest("m", "messages", []byte(body))
			m := decodeOut(t, out, eErr)
			if m["reasoning_effort"] != tc.want {
				t.Fatalf("reasoning_effort=%v want %q", m["reasoning_effort"], tc.want)
			}
		})
	}
	m := mustBuild(t, "messages", `{"max_tokens":64,"thinking":{"type":"disabled"}}`)
	if m["reasoning_effort"] != "none" {
		t.Fatalf("disabled thinking must set none: %v", m)
	}
}

// TestBuildRequestClaude verifies assistant reasoning and tools retain their
// message ownership when translating Messages history to Chat.
func TestBuildRequestClaude(t *testing.T) {
	body := `{
		"model":"x","max_tokens":256,"stream":true,"temperature":0.5,"top_p":0.9,
		"system":[{"type":"text","text":"sys"}],
		"stop_sequences":["END"],
		"messages":[
			{"role":"user","content":"hi"},
			{"role":"assistant","content":[{"type":"text","text":"hey"},{"type":"tool_use","id":"t1","name":"f","input":{"a":1}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"res"},{"type":"text","text":"more"}]},
			{"role":"assistant","content":[{"type":"thinking","thinking":"hmm"},{"type":"tool_use","id":"t2","name":"g"}]}
		],
		"tools":[{"name":"f","description":"d","input_schema":{"type":"object"}}]
	}`
	m := mustBuild(t, "messages", body)
	if m["model"] != "m" || m["stream"] != true || m["max_tokens"] != float64(256) ||
		m["temperature"] != 0.5 || m["top_p"] != 0.9 {
		t.Fatalf("envelope fields wrong: %v", m)
	}
	if fmt.Sprint(m["stop"]) != "[END]" {
		t.Fatalf("stop wrong: %v", m["stop"])
	}
	msgs := m["messages"].([]any)
	if len(msgs) != 6 {
		t.Fatalf("want 6 messages, got %d: %v", len(msgs), msgs)
	}
	sys := msgs[0].(map[string]any)
	if sys["role"] != "system" || sys["content"] != "sys" {
		t.Fatalf("system message wrong: %v", sys)
	}
	asst := msgs[2].(map[string]any)
	if asst["role"] != "assistant" || asst["content"] != "hey" {
		t.Fatalf("assistant message wrong: %v", asst)
	}
	calls := asst["tool_calls"].([]any)
	c0 := calls[0].(map[string]any)
	if c0["id"] != "t1" || c0["type"] != "function" {
		t.Fatalf("tool call wrong: %v", c0)
	}
	fn := c0["function"].(map[string]any)
	if fn["name"] != "f" || fn["arguments"] != `{"a":1}` {
		t.Fatalf("tool call function wrong: %v", fn)
	}
	tool := msgs[3].(map[string]any)
	if tool["role"] != "tool" || tool["tool_call_id"] != "t1" || tool["content"] != "res" {
		t.Fatalf("tool result message wrong: %v", tool)
	}
	user := msgs[4].(map[string]any)
	if user["content"] != "more" {
		t.Fatalf("post-result text wrong: %v", user)
	}
	// Thinking and tool_use share the assistant message; visible content is null.
	last := msgs[5].(map[string]any)
	if last["role"] != "assistant" || last["content"] != nil {
		t.Fatalf("thinking-only content wrong: %v", last)
	}
	lastCall := last["tool_calls"].([]any)[0].(map[string]any)
	if lastCall["id"] != "t2" || lastCall["function"].(map[string]any)["arguments"] != "{}" {
		t.Fatalf("missing-input tool call wrong: %v", lastCall)
	}
	tools := m["tools"].([]any)
	tl := tools[0].(map[string]any)
	if tl["type"] != "function" {
		t.Fatalf("tool type wrong: %v", tl)
	}
	tlf := tl["function"].(map[string]any)
	if tlf["name"] != "f" || tlf["description"] != "d" ||
		fmt.Sprint(tlf["parameters"]) != "map[type:object]" {
		t.Fatalf("tool function wrong: %v", tlf)
	}
}

func TestBuildRequestClaudeEmptyToolSchema(t *testing.T) {
	m := mustBuild(t, "messages", `{"tools":[{"name":"f"}],"messages":[{"role":"user","content":null}]}`)
	fn := m["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if fmt.Sprint(fn["parameters"]) != "map[properties:map[] type:object]" {
		t.Fatalf("schema default missing: %v", fn)
	}
	if msgs := m["messages"].([]any); len(msgs) != 0 {
		t.Fatalf("null content must not produce messages: %v", msgs)
	}
}

// shared.ClaudeMaxTokens defaults an absent or non-positive
// max_tokens defaults to 4096 instead of silently dropping the cap —
// the same policy the Responses leg applies.
func TestBuildRequestClaudeMaxTokensDefault(t *testing.T) {
	for _, body := range []string{
		`{"messages":[]}`,
		`{"max_tokens":0,"messages":[]}`,
		`{"max_tokens":-5,"messages":[]}`,
	} {
		m := mustBuild(t, "messages", body)
		if m["max_tokens"] != float64(4096) {
			t.Fatalf("%s: max_tokens = %v, want 4096", body, m["max_tokens"])
		}
	}
}

func TestBuildRequestClaudeErrors(t *testing.T) {
	cases := []struct{ name, body string }{
		{"malformed json", `{`},
		{"malformed block", `{"messages":[{"role":"user","content":[42]}]}`},
		{"user content wrong shape", `{"messages":[{"role":"user","content":42}]}`},
		{"assistant content wrong shape", `{"messages":[{"role":"assistant","content":42}]}`},
		{"image missing source", `{"messages":[{"role":"user","content":[{"type":"image"}]}]}`},
		{"source not an object", `{"messages":[{"role":"user","content":[{"type":"image","source":"x"}]}]}`},
		{"url source missing url", `{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"url"}}]}]}`},
		{"base64 missing data", `{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64"}}]}]}`},
		{"unknown source type", `{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"s3"}}]}]}`},
		{"result bad block type", `{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":[{"type":"video"}]}]}]}`},
		{"result malformed block", `{"messages":[{"role":"user","content":[{"type":"tool_result","content":[42]}]}]}`},
		{"result content wrong shape", `{"messages":[{"role":"user","content":[{"type":"tool_result","content":42}]}]}`},
		{"assistant malformed block", `{"messages":[{"role":"assistant","content":[42]}]}`},
		{"system bad block type", `{"system":[{"type":"doc"}]}`},
		{"system wrong shape", `{"system":123}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, eErr := BuildRequest("m", "messages", []byte(tc.body))
			if eErr == nil || eErr.Class != errclass.ClassTranslation {
				t.Fatalf("want ClassTranslation, got %+v", eErr)
			}
		})
	}
	// Unknown content-block types and unknown roles are
	// unsupported_protocol_or_parameter, matching the
	// Responses route.
	for _, c := range []struct{ name, body, typ string }{
		{"bad user block type", `{"messages":[{"role":"user","content":[{"type":"video"}]}]}`, `"video"`},
		{"bad assistant block type", `{"messages":[{"role":"assistant","content":[{"type":"video"}]}]}`, `"video"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, eErr := BuildRequest("m", "messages", []byte(c.body))
			if eErr == nil || eErr.Class != errclass.ClassUnsupported ||
				!strings.Contains(eErr.Message, c.typ) || !strings.Contains(eErr.Message, EndpointPath) {
				t.Fatalf("want ClassUnsupported naming type+endpoint, got %+v", eErr)
			}
		})
	}
	_, eErr := BuildRequest("m", "messages", []byte(`{"messages":[{"role":"robot","content":"x"}]}`))
	if eErr == nil || eErr.Class != errclass.ClassUnsupported ||
		!strings.Contains(eErr.Message, `"robot"`) {
		t.Fatalf("unsupported role: want ClassUnsupported, got %+v", eErr)
	}
}

func TestBuildRequestClaudeImageURLs(t *testing.T) {
	m := mustBuild(t, "messages",
		`{"messages":[{"role":"user","content":[{"type":"text","text":"a"},{"type":"image","source":{"type":"url","url":"https://x/i.png"}},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"QUJD"}},{"type":"image","source":{"type":"base64","data":"QUJD"}}]}]}`)
	parts := m["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if len(parts) != 4 || parts[0].(map[string]any)["type"] != "text" {
		t.Fatalf("parts wrong: %v", parts)
	}
	if got := parts[1].(map[string]any)["image_url"].(map[string]any)["url"]; got != "https://x/i.png" {
		t.Fatalf("plain url lost: %v", got)
	}
	if got := parts[2].(map[string]any)["image_url"].(map[string]any)["url"]; got != "data:image/png;base64,QUJD" {
		t.Fatalf("base64 not converted to data URL: %v", got)
	}
	if got := parts[3].(map[string]any)["image_url"].(map[string]any)["url"]; got != "data:application/octet-stream;base64,QUJD" {
		t.Fatalf("missing media_type not defaulted: %v", got)
	}
}

// TestBuildRequestClaudeVariants covers empty turns, readable thinking,
// opaque redacted blocks, and tool-result defaults.
func TestBuildRequestClaudeVariants(t *testing.T) {
	t.Run("string system field", func(t *testing.T) {
		m := mustBuild(t, "messages", `{"system":"plain","messages":[{"role":"user","content":"hi"}]}`)
		sys := m["messages"].([]any)[0].(map[string]any)
		if sys["role"] != "system" || sys["content"] != "plain" {
			t.Fatalf("string system lost: %v", sys)
		}
	})
	t.Run("multi-block system joined with blank lines", func(t *testing.T) {
		m := mustBuild(t, "messages",
			`{"system":[{"type":"text","text":"s1"},{"type":"text","text":"s2"}],"messages":[{"role":"user","content":"hi"}]}`)
		sys := m["messages"].([]any)[0].(map[string]any)
		if sys["role"] != "system" || sys["content"] != "s1\n\ns2" {
			t.Fatalf("system join wrong: %v", sys)
		}
	})
	t.Run("string assistant content", func(t *testing.T) {
		m := mustBuild(t, "messages", `{"messages":[{"role":"assistant","content":"str"}]}`)
		a := m["messages"].([]any)[0].(map[string]any)
		if a["role"] != "assistant" || a["content"] != "str" {
			t.Fatalf("assistant string content lost: %v", a)
		}
	})
	t.Run("thinking-only assistant preserved", func(t *testing.T) {
		m := mustBuild(t, "messages",
			`{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"hmm"}]}]}`)
		if msgs := m["messages"].([]any); len(msgs) != 1 || msgs[0].(map[string]any)["reasoning_content"] != "hmm" {
			t.Fatalf("thinking-only assistant lost: %v", msgs)
		}
	})
	// Omission policy: redacted_thinking is encrypted
	// reasoning metadata with no Chat Completions representable payload —
	// omitted while plain thinking survives (mirrors the
	// sibling Messages/Responses-route converters).
	t.Run("redacted_thinking omitted alongside thinking", func(t *testing.T) {
		m := mustBuild(t, "messages",
			`{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"hmm"},{"type":"redacted_thinking","data":"x"},{"type":"text","text":"answer"}]}]}`)
		msgs := m["messages"].([]any)
		if len(msgs) != 1 {
			t.Fatalf("want one assistant message, got %d: %v", len(msgs), msgs)
		}
		a := msgs[0].(map[string]any)
		if a["content"] != "answer" || a["reasoning_content"] != "hmm" {
			t.Fatalf("reasoning or text lost: %v", a)
		}
	})
	t.Run("redacted_thinking-only assistant preserved", func(t *testing.T) {
		m := mustBuild(t, "messages",
			`{"messages":[{"role":"assistant","content":[{"type":"redacted_thinking","data":"x"}]}]}`)
		if msgs := m["messages"].([]any); len(msgs) != 0 {
			t.Fatalf("redacted_thinking-only assistant not dropped: %v", msgs)
		}
	})
	t.Run("tool_result without content", func(t *testing.T) {
		m := mustBuild(t, "messages",
			`{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t9"}]}]}`)
		tool := m["messages"].([]any)[0].(map[string]any)
		if tool["role"] != "tool" || tool["tool_call_id"] != "t9" || tool["content"] != "" {
			t.Fatalf("contentless tool_result wrong: %v", tool)
		}
	})
	t.Run("tool_result block array content", func(t *testing.T) {
		m := mustBuild(t, "messages",
			`{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t9","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}]}]}`)
		tool := m["messages"].([]any)[0].(map[string]any)
		if tool["content"] != "ab" {
			t.Fatalf("block array result not flattened: %v", tool)
		}
	})
	t.Run("tool_result is_error marker", func(t *testing.T) {
		// is_error must survive as the "[error] " prefix like the
		// Responses-route twin (responses/request.go), never silently lost.
		m := mustBuild(t, "messages",
			`{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t9","content":"boom","is_error":true}]}]}`)
		tool := m["messages"].([]any)[0].(map[string]any)
		if tool["content"] != "[error] boom" {
			t.Fatalf("is_error marker missing: %v", tool)
		}
	})
}

// TestBuildRequestResponses checks the Responses envelope and the grouping
// of assistant text, parallel calls, and their tool results in Chat history.
func TestBuildRequestResponses(t *testing.T) {
	body := `{
		"model":"x","max_output_tokens":99,"temperature":0.7,
		"instructions":"be nice",
		"input":[
			{"type":"message","role":"developer","content":"dev"},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"q"},{"type":"input_image","image_url":{"url":"https://x/i.png"}}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"a"}]},
			{"type":"function_call","call_id":"c1","name":"f","arguments":"{\"x\":1}"},
			{"type":"function_call","call_id":"c2","name":"g","arguments":""},
			{"type":"function_call_output","call_id":"c1","output":"ok"},
			{"type":"reasoning","summary":[]}
		],
		"tools":[{"type":"function","name":"f","description":"d","parameters":{"type":"object"}}],
		"reasoning":{"effort":"HIGH"}
	}`
	m := mustBuild(t, "responses", body)
	if m["max_tokens"] != float64(99) || m["reasoning_effort"] != "high" || m["temperature"] != 0.7 {
		t.Fatalf("envelope fields wrong: %v", m)
	}
	msgs := m["messages"].([]any)
	if len(msgs) != 5 {
		t.Fatalf("want 5 messages, got %d: %v", len(msgs), msgs)
	}
	if m0 := msgs[0].(map[string]any); m0["role"] != "system" || m0["content"] != "be nice" {
		t.Fatalf("instructions message wrong: %v", m0)
	}
	if m1 := msgs[1].(map[string]any); m1["role"] != "system" || m1["content"] != "dev" {
		t.Fatalf("developer item wrong: %v", m1)
	}
	user := msgs[2].(map[string]any)["content"].([]any)
	if len(user) != 2 || user[1].(map[string]any)["type"] != "image_url" {
		t.Fatalf("user parts wrong: %v", user)
	}
	if msgs[3].(map[string]any)["content"] != "a" {
		t.Fatalf("assistant item wrong: %v", msgs[3])
	}
	merged := msgs[3].(map[string]any)
	calls := merged["tool_calls"].([]any)
	if merged["role"] != "assistant" || len(calls) != 2 {
		t.Fatalf("consecutive function_calls not merged: %v", merged)
	}
	c1 := calls[0].(map[string]any)
	if c1["id"] != "c1" || c1["function"].(map[string]any)["arguments"] != `{"x":1}` {
		t.Fatalf("first call wrong: %v", c1)
	}
	if calls[1].(map[string]any)["function"].(map[string]any)["arguments"] != "{}" {
		t.Fatalf("empty arguments not defaulted: %v", calls[1])
	}
	outMsg := msgs[4].(map[string]any)
	if outMsg["role"] != "tool" || outMsg["tool_call_id"] != "c1" || outMsg["content"] != "ok" {
		t.Fatalf("function_call_output wrong: %v", outMsg)
	}
	tools := m["tools"].([]any)
	tl := tools[0].(map[string]any)
	if tl["type"] != "function" || tl["function"].(map[string]any)["name"] != "f" {
		t.Fatalf("tool wrapping wrong: %v", tl)
	}
}

func TestFromResponsesRequest_FunctionCallOutputArray(t *testing.T) {
	body := `{
		"model":"opencode-go/glm-5.2",
		"input":[
			{"type":"function_call","call_id":"c1","name":"read","arguments":"{}"},
			{"type":"function_call_output","call_id":"c1","output":[{"type":"input_text","text":"file contents"}]}
		]
	}`
	m := mustBuild(t, "responses", body)
	msgs := m["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("want 2 messages, got %d: %v", len(msgs), msgs)
	}
	outMsg := msgs[1].(map[string]any)
	if outMsg["role"] != "tool" || outMsg["tool_call_id"] != "c1" || outMsg["content"] != "file contents" {
		t.Fatalf("function_call_output wrong: %v", outMsg)
	}
}

func TestBuildRequestResponsesVariants(t *testing.T) {
	t.Run("empty string input yields no messages", func(t *testing.T) {
		m := mustBuild(t, "responses", `{"input":""}`)
		if msgs := m["messages"].([]any); len(msgs) != 0 {
			t.Fatalf("unexpected messages: %v", msgs)
		}
	})
	t.Run("no input and no instructions", func(t *testing.T) {
		m := mustBuild(t, "responses", `{}`)
		if msgs := m["messages"].([]any); len(msgs) != 0 {
			t.Fatalf("unexpected messages: %v", msgs)
		}
	})
	t.Run("string input becomes user message", func(t *testing.T) {
		m := mustBuild(t, "responses", `{"input":"hi"}`)
		msgs := m["messages"].([]any)
		if len(msgs) != 1 || msgs[0].(map[string]any)["content"] != "hi" {
			t.Fatalf("wrong: %v", msgs)
		}
	})
	t.Run("zero max_output_tokens ignored", func(t *testing.T) {
		m := mustBuild(t, "responses", `{"max_output_tokens":0,"input":"hi"}`)
		if _, ok := m["max_tokens"]; ok {
			t.Fatalf("zero max_output_tokens must be omitted: %v", m)
		}
	})
	t.Run("parallel_tool_calls false forwarded", func(t *testing.T) {
		m := mustBuild(t, "responses", `{"input":"hi","parallel_tool_calls":false}`)
		if v, ok := m["parallel_tool_calls"].(bool); !ok || v {
			t.Fatalf("false parallel_tool_calls not forwarded: %v", m)
		}
	})
	t.Run("absent parallel_tool_calls omitted", func(t *testing.T) {
		m := mustBuild(t, "responses", `{"input":"hi"}`)
		if _, ok := m["parallel_tool_calls"]; ok {
			t.Fatalf("absent parallel_tool_calls must be omitted: %v", m)
		}
	})
	t.Run("plain string image url", func(t *testing.T) {
		m := mustBuild(t, "responses",
			`{"input":[{"type":"message","role":"user","content":[{"type":"input_image","image_url":"https://x/i.png"}]}]}`)
		content := m["messages"].([]any)[0].(map[string]any)["content"].([]any)
		if content[0].(map[string]any)["image_url"].(map[string]any)["url"] != "https://x/i.png" {
			t.Fatalf("plain-string image_url lost: %v", content)
		}
	})
	t.Run("null message content skipped", func(t *testing.T) {
		m := mustBuild(t, "responses",
			`{"input":[{"type":"message","role":"user","content":null},{"type":"message","role":"system","content":null}]}`)
		if msgs := m["messages"].([]any); len(msgs) != 0 {
			t.Fatalf("null content produced messages: %v", msgs)
		}
	})
	t.Run("empty string content skipped", func(t *testing.T) {
		m := mustBuild(t, "responses",
			`{"input":[{"type":"message","role":"user","content":""}]}`)
		if msgs := m["messages"].([]any); len(msgs) != 0 {
			t.Fatalf("empty content produced messages: %v", msgs)
		}
	})
	t.Run("empty parts array skipped", func(t *testing.T) {
		m := mustBuild(t, "responses",
			`{"input":[{"type":"message","role":"user","content":[]}]}`)
		if msgs := m["messages"].([]any); len(msgs) != 0 {
			t.Fatalf("empty parts produced messages: %v", msgs)
		}
	})
	t.Run("system text parts joined", func(t *testing.T) {
		m := mustBuild(t, "responses",
			`{"input":[{"type":"message","role":"developer","content":[{"type":"input_text","text":"a"},{"type":"output_text","text":"b"}]}]}`)
		sys := m["messages"].([]any)[0].(map[string]any)
		if sys["role"] != "system" || sys["content"] != "ab" {
			t.Fatalf("system parts wrong: %v", sys)
		}
	})
	t.Run("tool without parameters gets default schema", func(t *testing.T) {
		m := mustBuild(t, "responses",
			`{"tools":[{"type":"function","name":"f"}],"input":"hi"}`)
		fn := m["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
		if fmt.Sprint(fn["parameters"]) != "map[properties:map[] type:object]" {
			t.Fatalf("schema default missing: %v", fn)
		}
	})
}

func TestBuildRequestResponsesErrors(t *testing.T) {
	cases := []struct{ name, body string }{
		{"malformed json", `{`},
		{"instructions not a string", `{"instructions":5}`},
		{"input wrong shape", `{"input":5}`},
		{"system with image part", `{"input":[{"type":"message","role":"system","content":[{"type":"input_image","image_url":{"url":"https://x"}}]}]}`},
		{"image missing url", `{"input":[{"type":"message","role":"user","content":[{"type":"input_image"}]}]}`},
		{"malformed content", `{"input":[{"type":"message","role":"user","content":{"bad":1}}]}`},
		{"developer content wrong shape", `{"input":[{"type":"message","role":"developer","content":{"bad":1}}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, eErr := BuildRequest("m", "responses", []byte(tc.body))
			if eErr == nil || eErr.Class != errclass.ClassTranslation {
				t.Fatalf("want ClassTranslation, got %+v", eErr)
			}
		})
	}
	// Unknown roles, content part types, and input item types are
	// unsupported_protocol_or_parameter, not translation.
	for _, tc := range []struct{ name, body string }{
		{"unsupported role", `{"input":[{"type":"message","role":"robot","content":"x"}]}`},
		{"unsupported part type", `{"input":[{"type":"message","role":"user","content":[{"type":"audio"}]}]}`},
		{"unsupported item type", `{"input":[{"type":"web_search"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, eErr := BuildRequest("m", "responses", []byte(tc.body))
			if eErr == nil || eErr.Class != errclass.ClassUnsupported {
				t.Fatalf("want ClassUnsupported, got %+v", eErr)
			}
		})
	}
	// Hosted web search tools are dropped on the Chat Completions route
	// (no function-calling equivalent); function tools proceed.
	m := mustBuild(t, "responses",
		`{"tools":[{"type":"web_search"},{"type":"web_search_preview"},{"type":"function","name":"f"}],"input":"hi"}`)
	tools, ok := m["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("web_search not dropped: %v", m["tools"])
	}
	if tools[0].(map[string]any)["function"].(map[string]any)["name"] != "f" {
		t.Fatalf("function tool lost: %v", m["tools"])
	}
}

func TestFromResponses_NamespaceChildUnrolls(t *testing.T) {
	body := `{
		"input": "hi",
		"tools": [
			{"type": "namespace", "name": "subagents", "tools": [
				{"type": "function", "name": "spawn_agent", "description": "d", "parameters": {"type": "object"}}
			]}
		]
	}`
	out, eErr := BuildRequest("m", "responses", []byte(body), shared.NewConversionContext())
	m := decodeOut(t, out, eErr)
	tools, ok := m["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("expected 1 flattened tool, got %+v", m["tools"])
	}
	fn := tools[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "subagents__spawn_agent" {
		t.Fatalf("expected flattened subagents__spawn_agent, got %v", fn["name"])
	}
}

func TestFromResponses_NamespacedToolChoiceFlattened(t *testing.T) {
	body := `{
		"input": "hi",
		"tools": [
			{"type": "namespace", "name": "subagents", "tools": [
				{"type": "function", "name": "spawn_agent", "description": "d", "parameters": {"type": "object"}}
			]}
		],
		"tool_choice": {"type": "function", "name": "spawn_agent", "namespace": "subagents"}
	}`
	out, eErr := BuildRequest("m", "responses", []byte(body), shared.NewConversionContext())
	m := decodeOut(t, out, eErr)
	tc, ok := m["tool_choice"].(map[string]any)
	if !ok || tc["type"] != "function" {
		t.Fatalf("tool_choice wrong shape: %v", m["tool_choice"])
	}
	fn, ok := tc["function"].(map[string]any)
	if !ok || fn["name"] != "subagents__spawn_agent" {
		t.Fatalf("expected flattened subagents__spawn_agent, got %v", m["tool_choice"])
	}
}

func TestFromResponses_NamespaceToolIgnored(t *testing.T) {
	body := `{
		"input": "hello",
		"tools": [
			{"type": "function", "name": "f1", "parameters": {"type": "object"}},
			{"type": "namespace", "name": "subagents", "tools": []}
		]
	}`
	out := mustBuild(t, "responses", body)
	tools, ok := out["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %+v", out["tools"])
	}
	toolMap := tools[0].(map[string]any)
	fn := toolMap["function"].(map[string]any)
	if fn["name"] != "f1" {
		t.Fatalf("expected tool name f1, got %v", fn["name"])
	}
}

func TestResponsesReasoningEffortValidated(t *testing.T) {
	// Transparent router: unlisted efforts pass through without local
	// validation; upstream is the sole authority.
	out := mustBuild(t, "responses", `{"reasoning":{"effort":"xhigh"},"input":"hi"}`)
	if out["reasoning_effort"] != "xhigh" {
		t.Fatalf("passthrough effort = %v, want xhigh", out["reasoning_effort"])
	}
	out = mustBuild(t, "responses", `{"reasoning":{"effort":"high"},"input":"hi"}`)
	if out["reasoning_effort"] != "high" {
		t.Fatalf("supported effort = %v", out["reasoning_effort"])
	}

	// Mixed-case effort is normalized.

	out = mustBuild(t, "responses", `{"reasoning":{"effort":"XHigh"},"input":"hi"}`)
	if out["reasoning_effort"] != "xhigh" {
		t.Fatalf("normalized effort = %v", out["reasoning_effort"])
	}
}

// F-drift pins: tool_choice preserved on both CC-target cross-format legs.
func TestClaudeSourceToolChoicePreserved(t *testing.T) {
	m := mustBuild(t, "messages",
		`{"tool_choice":{"type":"auto","name":""},"messages":[{"role":"user","content":"hi"}]}`)
	if m["tool_choice"] != "auto" {
		t.Fatalf("auto tool_choice = %v", m["tool_choice"])
	}
	m = mustBuild(t, "messages",
		`{"tool_choice":{"type":"tool","name":"lookup"},"messages":[{"role":"user","content":"hi"}]}`)
	tc, ok := m["tool_choice"].(map[string]any)
	if !ok || tc["type"] != "function" ||
		tc["function"].(map[string]any)["name"] != "lookup" {
		t.Fatalf("named tool_choice = %v", m["tool_choice"])
	}
	m = mustBuild(t, "messages",
		`{"tool_choice":{"type":"any"},"messages":[]}`)
	if m["tool_choice"] != "required" {
		t.Fatalf("any tool_choice = %v", m["tool_choice"])
	}
}

func TestResponsesSourceToolChoicePreserved(t *testing.T) {
	m := mustBuild(t, "responses",
		`{"tool_choice":"auto","input":"hi"}`)
	if m["tool_choice"] != "auto" {
		t.Fatalf("auto tool_choice = %v", m["tool_choice"])
	}
	m = mustBuild(t, "responses",
		`{"tool_choice":{"type":"function","name":"lookup"},"input":"hi"}`)
	tc, ok := m["tool_choice"].(map[string]any)
	if !ok || tc["type"] != "function" ||
		tc["function"].(map[string]any)["name"] != "lookup" {
		t.Fatalf("named tool_choice = %v", m["tool_choice"])
	}
}

// tool_choice "none" is natively representable on CC-target legs and is
// preserved instead of rejected.

func TestClaudeSourceNoneToolChoicePreserved(t *testing.T) {
	m := mustBuild(t, "messages",
		`{"tool_choice":{"type":"none","name":""},"messages":[]}`)
	if m["tool_choice"] != "none" {
		t.Fatalf("none tool_choice = %v", m["tool_choice"])
	}
}

func TestResponsesSourceNoneToolChoicePreserved(t *testing.T) {
	m := mustBuild(t, "responses",
		`{"tool_choice":"none","input":"hi"}`)
	if m["tool_choice"] != "none" {
		t.Fatalf("none tool_choice = %v", m["tool_choice"])
	}
}

// applyToolChoiceCC maps every normalized kind onto the CC wire format.

func TestApplyToolChoiceCC(t *testing.T) {
	cases := []struct {
		kind, name string
		want       any // nil asserts the field stays absent
	}{
		{shared.ToolChoiceAbsent, "", nil},
		{shared.ToolChoiceAuto, "", "auto"},
		{shared.ToolChoiceAny, "", "required"},
		{shared.ToolChoiceNone, "", "none"},
	}
	for _, tc := range cases {
		req := &ccRequest{}
		applyToolChoiceCC(req, tc.kind, tc.name)
		b, _ := json.Marshal(req)
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("%s: %v", tc.kind, err)
		}
		got, ok := m["tool_choice"]
		if got != tc.want || ok != (tc.want != nil) {
			t.Fatalf("%s: tool_choice = %v (present=%t), want %v", tc.kind, got, ok, tc.want)
		}
	}

	req := &ccRequest{}
	applyToolChoiceCC(req, shared.ToolChoiceNamed, "lookup")
	var named struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(req.ToolChoice, &named); err != nil ||
		named.Type != "function" || named.Function.Name != "lookup" {
		t.Fatalf("named tool_choice = %s", req.ToolChoice)
	}
}

// Exhaustiveness guard: applyToolChoiceCC is infallible because
// shared.DecodeToolChoice emits a closed set. Every kind in
// shared.AllToolChoiceKinds except absent must render a wire value; a new
// kind added to the slice without extending the renderer falls through to
// nothing and fails here.
func TestApplyToolChoiceCCKindsExhaustive(t *testing.T) {
	for _, kind := range shared.AllToolChoiceKinds {
		req := &ccRequest{}
		applyToolChoiceCC(req, kind, "probe")
		if kind == shared.ToolChoiceAbsent {
			if req.ToolChoice != nil {
				t.Fatalf("%s: tool_choice = %s, want absent", kind, req.ToolChoice)
			}
			continue
		}
		if req.ToolChoice == nil {
			t.Fatalf("kind %q rendered no tool_choice; extend applyToolChoiceCC", kind)
		}
	}
}

// Malformed tool_choice fails translation on both CC-target legs instead
// of being silently dropped.
func TestMalformedToolChoiceRejected(t *testing.T) {
	for _, format := range []string{"messages", "responses"} {
		body := `{"tool_choice":42,"messages":[]}`
		if format == "responses" {
			body = `{"tool_choice":42,"input":"hi"}`
		}
		_, eErr := BuildRequest("m", format, []byte(body))
		if eErr == nil || !strings.Contains(eErr.Message, "malformed tool_choice") {
			t.Fatalf("%s: err = %+v", format, eErr)
		}
	}
}

func TestInHistorySystemRoleInMessages(t *testing.T) {
	m := mustBuild(t, "messages", `{"messages":[{"role":"system","content":"sys-inturn"},{"role":"user","content":"hi"}]}`)
	msgs := m["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("want 2 messages, got %d: %v", len(msgs), msgs)
	}
	sys := msgs[0].(map[string]any)
	if sys["role"] != "system" || sys["content"] != "sys-inturn" {
		t.Fatalf("in-history system message wrong: %v", sys)
	}
}

func TestFromResponses_CustomToolCallAndOutput(t *testing.T) {
	body := `{
		"input": [
			{"type": "message", "role": "user", "content": "run script"},
			{"type": "custom_tool_call", "call_id": "call_c1", "name": "exec", "input": "text('ok');"},
			{"type": "custom_tool_call_output", "call_id": "call_c1", "output": [{"type": "input_text", "text": "ok"}]}
		]
	}`
	out, eErr := BuildRequest("gpt-4o", "responses", []byte(body))
	if eErr != nil {
		t.Fatalf("unexpected error: %v", eErr)
	}
	m := decodeOut(t, out, eErr)
	msgs := m["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("want 3 messages, got %d: %v", len(msgs), msgs)
	}
	if msgs[0].(map[string]any)["role"] != "user" || msgs[0].(map[string]any)["content"] != "run script" {
		t.Fatalf("message 0 wrong: %v", msgs[0])
	}
	asst := msgs[1].(map[string]any)
	if asst["role"] != "assistant" {
		t.Fatalf("message 1 role = %v, want assistant", asst["role"])
	}
	calls := asst["tool_calls"].([]any)
	c0 := calls[0].(map[string]any)
	if c0["id"] != "call_c1" {
		t.Fatalf("tool call id = %v, want call_c1", c0["id"])
	}
	fn := c0["function"].(map[string]any)
	if fn["name"] != "exec" || fn["arguments"] != "text('ok');" {
		t.Fatalf("tool call function wrong: %v", fn)
	}
	tool := msgs[2].(map[string]any)
	if tool["role"] != "tool" || tool["tool_call_id"] != "call_c1" || tool["content"] != "ok" {
		t.Fatalf("tool message wrong: %v", tool)
	}
}

func TestResponsesReasoningEffortPassthroughWithoutValidation(t *testing.T) {
	body := `{"reasoning":{"effort":"xhigh"},"input":"hi"}`
	out, eErr := BuildRequest("gpt-4o", "responses", []byte(body))
	if eErr != nil {
		t.Fatalf("BuildRequest err = %+v, want nil (passthrough xhigh)", eErr)
	}
	m := decodeOut(t, out, nil)
	if m["reasoning_effort"] != "xhigh" {
		t.Fatalf("reasoning_effort = %v, want xhigh", m["reasoning_effort"])
	}
}
