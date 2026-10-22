package adapters

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/dillonzq/apix/internal/errclass"
	"github.com/dillonzq/apix/internal/llm/adapter/chatcompletions"
	"github.com/dillonzq/apix/internal/llm/adapter/messages"
	"github.com/dillonzq/apix/internal/llm/adapter/responses"
)

func reasoningRequest(t *testing.T, target, source string, body []byte) (map[string]any, *errclass.Error) {
	t.Helper()
	var out []byte
	var eErr *errclass.Error
	switch target {
	case "chat-completions":
		out, eErr = chatcompletions.BuildRequest("m", source, body)
	case "responses":
		out, eErr = responses.BuildRequest("m", source, body)
	case "messages":
		out, eErr = messages.BuildRequest("m", source, body)
	}
	if eErr != nil {
		return nil, eErr
	}
	var req map[string]any
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatal(err)
	}
	return req, nil
}

// Reasoning conversion preserves client controls in streaming and non-streaming requests.
func TestReasoningConversionControls(t *testing.T) {
	for _, source := range []string{"chat-completions", "responses"} {
		for _, target := range []string{"chat-completions", "responses", "messages"} {
			for _, effort := range []string{"none", "auto", "minimal", "xhigh", "max", "turbo"} {
				for _, stream := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/%s/%s/stream=%t", source, target, effort, stream), func(t *testing.T) {
						body := map[string]any{"stream": stream, "temperature": 0.5, "top_p": 0.9}
						if source == "chat-completions" {
							body["messages"], body["max_tokens"], body["reasoning_effort"] = []any{}, 64, effort
						} else {
							body["input"], body["max_output_tokens"], body["reasoning"] = []any{}, 64, map[string]any{"effort": effort}
						}
						raw, _ := json.Marshal(body)

						req, eErr := reasoningRequest(t, target, source, raw)
						if target == "messages" && effort == "turbo" {
							if eErr == nil || eErr.Class != errclass.ClassUnsupported {
								t.Fatalf("unconvertible effort silently lost: %v", eErr)
							}
							return
						}
						if eErr != nil {
							t.Fatal(eErr)
						}

						if req["temperature"] != 0.5 || req["top_p"] != 0.9 || (req["stream"] == true) != stream {
							t.Fatalf("client controls changed: %v", req)
						}
						switch target {
						case "chat-completions":
							if effort == "auto" && source != target {
								if _, has := req["reasoning_effort"]; has {
									t.Fatalf("auto must use defaults: %v", req)
								}
								return
							}
							if req["reasoning_effort"] != effort {
								t.Fatalf("effort lost: %v", req)
							}
						case "responses":
							if effort == "auto" && source != target {
								if _, has := req["reasoning"]; has {
									t.Fatalf("auto must use defaults: %v", req)
								}
								return
							}
							if req["reasoning"].(map[string]any)["effort"] != effort {
								t.Fatalf("effort lost: %v", req)
							}
						case "messages":
							if effort == "auto" {
								if _, has := req["thinking"]; has {
									t.Fatalf("auto must use defaults: %v", req)
								}
								return
							}
							th := req["thinking"].(map[string]any)
							wantType := "enabled"
							if effort == "none" {
								wantType = "disabled"
							}
							if th["type"] != wantType || req["max_tokens"] != float64(64) {
								t.Fatalf("thinking/limit changed: %v", req)
							}
							wantBudget := map[string]any{"minimal": float64(512), "xhigh": float64(32768), "max": float64(128000)}[effort]
							if th["budget_tokens"] != wantBudget {
								t.Fatalf("budget changed: %v", th)
							}
						}

					})
				}
			}
		}
	}
}

func TestClaudeReasoningControlsAcrossFormats(t *testing.T) {
	for _, target := range []string{"chat-completions", "responses"} {
		for _, stream := range []bool{false, true} {
			for _, tc := range []struct{ control, effort string }{
				{``, ""},
				{`,"thinking":{"type":"disabled","budget_tokens":8192}`, "none"},
				{`,"thinking":{"type":"enabled"}`, "auto"},
				{`,"thinking":{"type":"enabled","budget_tokens":0}`, "none"},
				{`,"thinking":{"type":"enabled","budget_tokens":-1}`, "auto"},
				{`,"thinking":{"type":"enabled","budget_tokens":512}`, "minimal"},
				{`,"thinking":{"type":"enabled","budget_tokens":8192}`, "medium"},
				{`,"thinking":{"type":"enabled","budget_tokens":128000}`, "xhigh"},
				{`,"thinking":{"type":"adaptive"}`, "auto"},
				{`,"thinking":{"type":"adaptive"},"output_config":{"effort":"max"}`, "max"},
				{`,"thinking":{"type":"adaptive"},"output_config":{"effort":"turbo"}`, "turbo"},
			} {
				t.Run(fmt.Sprintf("%s/%s/stream=%t", target, tc.control, stream), func(t *testing.T) {
					body := []byte(fmt.Sprintf(`{"messages":[],"max_tokens":64,"stream":%t%s}`, stream, tc.control))

					req, eErr := reasoningRequest(t, target, "messages", body)
					if eErr != nil {
						t.Fatal(eErr)
					}
					var effort any
					if target == "chat-completions" {
						effort = req["reasoning_effort"]
					} else if r, ok := req["reasoning"].(map[string]any); ok {
						effort = r["effort"]
					}
					if tc.effort == "" || tc.effort == "auto" {
						if effort != nil {
							t.Fatalf("default thinking became explicit effort %v", effort)
						}
					} else if effort != tc.effort {
						t.Fatalf("effort = %v, want %s", effort, tc.effort)
					}

				})
			}
			for _, control := range []string{`{"type":"enabled","budget_tokens":-2}`, `{"type":"future"}`} {
				_, eErr := reasoningRequest(t, target, "messages", []byte(fmt.Sprintf(`{"stream":%t,"thinking":%s}`, stream, control)))
				if eErr == nil || eErr.Class != errclass.ClassUnsupported {
					t.Fatalf("invalid control silently lost: %v", eErr)
				}
			}
		}
	}
}

func TestNativeReasoningPreserved(t *testing.T) {
	for source, control := range map[string]string{
		"chat-completions": `"reasoning_effort":"turbo","thinking":{"type":"future","budget_tokens":-2}`,
		"responses":        `"reasoning":{"effort":"turbo","summary":"detailed","future":true}`,
		"messages":         `"thinking":{"type":"adaptive","future":true},"output_config":{"effort":"turbo","future":true}`,
	} {
		for _, stream := range []bool{false, true} {
			raw := []byte(fmt.Sprintf(`{"model":"old","messages":[],"stream":%t,%s}`, stream, control))
			req, eErr := reasoningRequest(t, source, source, raw)
			if eErr != nil {
				t.Fatal(eErr)
			}
			var original map[string]any
			json.Unmarshal(raw, &original)
			for _, field := range []string{"reasoning_effort", "thinking", "reasoning", "output_config"} {
				if !reflect.DeepEqual(original[field], req[field]) {
					t.Fatalf("native %s %s changed: %v vs %v", source, field, req[field], original[field])
				}
			}
		}
	}
}
