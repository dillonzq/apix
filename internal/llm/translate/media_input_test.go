package translate_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/dillonzq/apix/internal/llm/translate"
)

func mediaRequest(format, role, parts string) []byte {
	if format == translate.FormatResponses {
		return []byte(`{"model":"client","input":[{"role":"` + role + `","content":` + parts + `}]}`)
	}
	return []byte(`{"model":"client","messages":[{"role":"` + role + `","content":` + parts + `}]}`)
}

func mediaContent(t *testing.T, format string, body []byte) any {
	t.Helper()
	doc := object(t, body)
	if doc["model"] != "upstream" {
		t.Fatalf("model was not rewritten: %s", body)
	}
	key := "messages"
	if format == translate.FormatResponses {
		key = "input"
	}
	return doc[key].([]any)[0].(map[string]any)["content"]
}

func TestMediaInputConversions(t *testing.T) {
	const chatFile = `{"type":"file","file":{"file_data":"data:application/pdf;base64,JVBERg==","filename":"a.pdf"}}`
	const respFile = `{"type":"input_file","file_data":"data:application/pdf;base64,JVBERg==","filename":"a.pdf"}`
	const document = `{"type":"document","title":"a.pdf","source":{"type":"base64","media_type":"application/pdf","data":"JVBERg=="}}`
	const chatAudio = `{"type":"input_audio","input_audio":{"data":"UklGRg==","format":"wav"}}`
	const chatVideo = `{"type":"video_url","video_url":{"url":"data:video/mp4;base64,AAAA","processing":"static"}}`
	const respVideo = `{"type":"input_video","video_url":{"url":"data:video/mp4;base64,AAAA","processing":"static"}}`
	for _, tc := range []struct{ name, from, to, input, want string }{
		{"chat files to responses", translate.FormatChatCompletions, translate.FormatResponses, chatFile, respFile},
		{"responses files to chat", translate.FormatResponses, translate.FormatChatCompletions, respFile, chatFile},
		{"chat files to messages", translate.FormatChatCompletions, translate.FormatMessages, chatFile, document},
		{"responses files to messages", translate.FormatResponses, translate.FormatMessages, respFile, document},
		{"messages files to chat", translate.FormatMessages, translate.FormatChatCompletions, document, chatFile},
		{"messages files to responses", translate.FormatMessages, translate.FormatResponses, document, respFile},
		{"chat audio to responses", translate.FormatChatCompletions, translate.FormatResponses, chatAudio, chatAudio},
		{"responses audio to chat", translate.FormatResponses, translate.FormatChatCompletions, chatAudio, chatAudio},
		{"flat responses audio", translate.FormatResponses, translate.FormatChatCompletions, `{"type":"input_audio","data":"UklGRg==","format":"wav"}`, chatAudio},
		{"chat video to responses", translate.FormatChatCompletions, translate.FormatResponses, chatVideo, respVideo},
		{"responses video to chat", translate.FormatResponses, translate.FormatChatCompletions, respVideo, chatVideo},
		{"video string and processing override", translate.FormatResponses, translate.FormatChatCompletions, `{"type":"input_video","video_url":"data:video/mp4;base64,AAAA","processing":"static"}`, chatVideo},
		{"video nested processing override", translate.FormatResponses, translate.FormatChatCompletions, `{"type":"input_video","video_url":{"url":"data:video/mp4;base64,AAAA","processing":"agentic"},"processing":"static"}`, chatVideo},
		{"file id to responses", translate.FormatChatCompletions, translate.FormatResponses, `{"type":"file","file":{"file_id":"file-1"}}`, `{"type":"input_file","file_id":"file-1"}`},
		{"file id to chat", translate.FormatResponses, translate.FormatChatCompletions, `{"type":"input_file","file_id":"file-1"}`, `{"type":"file","file":{"file_id":"file-1"}}`},
		{"file URL to messages", translate.FormatResponses, translate.FormatMessages, `{"type":"input_file","file_url":"https://example.test/a.pdf"}`, `{"type":"document","source":{"type":"url","url":"https://example.test/a.pdf"}}`},
		{"document URL to responses", translate.FormatMessages, translate.FormatResponses, `{"type":"document","source":{"type":"url","url":"https://example.test/a.pdf"}}`, `{"type":"input_file","file_url":"https://example.test/a.pdf"}`},
		{"text document", translate.FormatMessages, translate.FormatResponses, `{"type":"document","source":{"type":"text","data":"hello"}}`, `{"type":"input_file","file_data":"data:text/plain;base64,aGVsbG8="}`},
		{"chat text file to messages", translate.FormatChatCompletions, translate.FormatMessages, `{"type":"file","file":{"file_data":"data:text/plain;base64,aGVsbG8=","filename":"a.txt"}}`, `{"type":"document","title":"a.txt","source":{"type":"text","media_type":"text/plain","data":"hello"}}`},
		{"responses text file to messages", translate.FormatResponses, translate.FormatMessages, `{"type":"input_file","file_data":"data:text/plain;base64,aGVsbG8=","filename":"a.txt"}`, `{"type":"document","title":"a.txt","source":{"type":"text","media_type":"text/plain","data":"hello"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := translate.NewRequestState()
			state.Options.LossPolicy = translate.LossPolicyStrict
			textType, targetText := "text", "text"
			if tc.from == translate.FormatResponses {
				textType = "input_text"
			}
			if tc.to == translate.FormatResponses {
				targetText = "input_text"
			}
			input := `[{"type":"` + textType + `","text":"before"},` + tc.input + `,{"type":"` + textType + `","text":"after"}]`
			body, err := translate.ConvertRequest(tc.to, "upstream", tc.from, mediaRequest(tc.from, "user", input), state)
			if err != nil {
				t.Fatal(err)
			}
			var want any
			json.Unmarshal([]byte(`[{"type":"`+targetText+`","text":"before"},`+tc.want+`,{"type":"`+targetText+`","text":"after"}]`), &want)
			if got := mediaContent(t, tc.to, body); !reflect.DeepEqual(got, want) {
				t.Fatalf("content = %v, want %v", got, want)
			}
			if len(state.Diagnostics()) != 0 {
				t.Fatalf("unexpected losses: %v", state.Diagnostics())
			}
		})
	}
}

func TestMediaInputRejections(t *testing.T) {
	for _, tc := range []struct {
		name, from, to, role, part string
		class                      translate.Class
	}{
		{"messages audio", translate.FormatChatCompletions, translate.FormatMessages, "user", `{"type":"input_audio","input_audio":{"data":"AAAA","format":"wav"}}`, translate.ClassUnsupported},
		{"messages video", translate.FormatResponses, translate.FormatMessages, "user", `{"type":"input_video","video_url":"https://example.test/a.mp4"}`, translate.ClassUnsupported},
		{"messages file id", translate.FormatResponses, translate.FormatMessages, "user", `{"type":"input_file","file_id":"file-1"}`, translate.ClassUnsupported},
		{"chat URL file", translate.FormatResponses, translate.FormatChatCompletions, "user", `{"type":"input_file","file_url":"https://example.test/a.pdf"}`, translate.ClassUnsupported},
		{"chat document URL", translate.FormatMessages, translate.FormatChatCompletions, "user", `{"type":"document","source":{"type":"url","url":"https://example.test/a.pdf"}}`, translate.ClassUnsupported},
		{"missing file", translate.FormatChatCompletions, translate.FormatResponses, "user", `{"type":"file"}`, translate.ClassTranslation},
		{"empty file", translate.FormatResponses, translate.FormatChatCompletions, "user", `{"type":"input_file"}`, translate.ClassTranslation},
		{"audio missing data", translate.FormatResponses, translate.FormatChatCompletions, "user", `{"type":"input_audio","format":"wav"}`, translate.ClassTranslation},
		{"video numeric URL", translate.FormatResponses, translate.FormatChatCompletions, "user", `{"type":"input_video","video_url":123}`, translate.ClassTranslation},
		{"video missing URL", translate.FormatResponses, translate.FormatChatCompletions, "user", `{"type":"input_video"}`, translate.ClassTranslation},
		{"document missing data", translate.FormatMessages, translate.FormatResponses, "user", `{"type":"document","source":{"type":"base64"}}`, translate.ClassTranslation},
		{"document invalid base64", translate.FormatMessages, translate.FormatResponses, "user", `{"type":"document","source":{"type":"base64","data":"!"}}`, translate.ClassTranslation},
		{"file invalid base64", translate.FormatResponses, translate.FormatMessages, "user", `{"type":"input_file","file_data":"data:application/pdf;base64,!"}`, translate.ClassTranslation},
		{"file malformed data URL", translate.FormatResponses, translate.FormatMessages, "user", `{"type":"input_file","file_data":"data:application/pdf,abc"}`, translate.ClassTranslation},
		{"document unsupported source", translate.FormatMessages, translate.FormatResponses, "user", `{"type":"document","source":{"type":"content"}}`, translate.ClassUnsupported},
		{"system file", translate.FormatResponses, translate.FormatChatCompletions, "system", `{"type":"input_file","file_id":"file-1"}`, translate.ClassUnsupported},
		{"assistant file to responses", translate.FormatChatCompletions, translate.FormatResponses, "assistant", `{"type":"file","file":{"file_id":"file-1"}}`, translate.ClassUnsupported},
		{"assistant document to messages", translate.FormatResponses, translate.FormatMessages, "assistant", `{"type":"input_file","file_data":"data:application/pdf;base64,JVBERg=="}`, translate.ClassUnsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := translate.ConvertRequest(tc.to, "upstream", tc.from, mediaRequest(tc.from, tc.role, `[{"type":"text","text":"keep"},`+tc.part+`]`), translate.NewRequestState())
			if err == nil || err.Class != tc.class || body != nil {
				t.Fatalf("body=%s error=%v, want %s", body, err, tc.class)
			}
		})
	}
}

func TestMediaNativeRequestsPreserveFields(t *testing.T) {
	for _, format := range formats {
		input := mediaRequest(format, "user", `[{"type":"vendor_media","data":"opaque"}]`)
		body, err := translate.ConvertRequest(format, "upstream", format, input, nil)
		if err != nil {
			t.Fatal(err)
		}
		got, want := object(t, body), object(t, input)
		want["model"] = "upstream"
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("native media changed: %s", body)
		}
	}
}

func TestDocumentMetadataLossPolicy(t *testing.T) {
	input := mediaRequest(translate.FormatMessages, "user", `[{"type":"document","context":"context","citations":{"enabled":true},"source":{"type":"base64","data":"JVBERg==","media_type":"application/pdf"}}]`)
	for _, policy := range []translate.LossPolicy{translate.LossPolicyAllow, translate.LossPolicySafe, translate.LossPolicyStrict} {
		state := translate.NewRequestState()
		state.Options.LossPolicy = policy
		_, err := translate.ConvertRequest(translate.FormatResponses, "upstream", translate.FormatMessages, input, state)
		if (err == nil) != (policy == translate.LossPolicyAllow) {
			t.Fatalf("policy %s: %v", policy, err)
		}
		if ds := state.Diagnostics(); len(ds) != 2 || ds[0].Path != "messages[0].content[0].citations" || !ds[0].Semantic {
			t.Fatalf("diagnostics: %v", ds)
		}
	}
}

func TestResponsesToolMediaDoesNotDisappear(t *testing.T) {
	_, err := translate.ConvertRequest(translate.FormatChatCompletions, "upstream", translate.FormatResponses, []byte(`{"input":[{"type":"function_call_output","call_id":"c","output":[{"type":"input_file","file_id":"file-1"}]}]}`), translate.NewRequestState())
	if err == nil || err.Class != translate.ClassUnsupported {
		t.Fatalf("tool file should be rejected: %v", err)
	}
}

func TestMixedMediaInputRoundTrip(t *testing.T) {
	input := mediaRequest(translate.FormatChatCompletions, "user", `[{"type":"text","text":"before"},{"type":"image_url","image_url":{"url":"https://example.test/a.png"}},{"type":"file","file":{"filename":"a.pdf","file_data":"data:application/pdf;base64,JVBERg=="}},{"type":"input_audio","input_audio":{"data":"UklGRg==","format":"wav"}},{"type":"video_url","video_url":{"url":"https://example.test/a.mp4?x=1&y=2","processing":"agentic"}},{"type":"text","text":"after"}]`)
	state := translate.NewRequestState()
	state.Options.LossPolicy = translate.LossPolicyStrict
	responses, err := translate.ConvertRequest(translate.FormatResponses, "upstream", translate.FormatChatCompletions, input, state)
	if err != nil {
		t.Fatal(err)
	}
	chat, err := translate.ConvertRequest(translate.FormatChatCompletions, "upstream", translate.FormatResponses, responses, state)
	if err != nil {
		t.Fatal(err)
	}
	want := object(t, input)["messages"].([]any)[0].(map[string]any)["content"]
	if got := mediaContent(t, translate.FormatChatCompletions, chat); !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip content = %v, want %v", got, want)
	}
}

func TestMediaResponseRemainsUnsupported(t *testing.T) {
	_, err := translate.ConvertResponse(translate.FormatChatCompletions, translate.FormatMessages, 200, []byte(`{"choices":[{"message":{"role":"assistant","content":[{"type":"file","file":{"file_data":"data:application/pdf;base64,JVBERg=="}}]},"finish_reason":"stop"}]}`), nil)
	if err == nil || err.Class != translate.ClassUnsupported {
		t.Fatalf("file output should be rejected: %v", err)
	}
}

func TestMessagesRejectsUnsupportedFileMIME(t *testing.T) {
	for _, from := range []string{translate.FormatChatCompletions, translate.FormatResponses} {
		for _, tc := range []struct {
			name, data string
			class      translate.Class
		}{
			{"missing MIME", "JVBERg==", translate.ClassUnsupported},
			{"octet stream", "data:application/octet-stream;base64,JVBERg==", translate.ClassUnsupported},
			{"image", "data:image/png;base64,JVBERg==", translate.ClassUnsupported},
			{"unknown MIME", "data:application/x-unknown;base64,JVBERg==", translate.ClassUnsupported},
			{"invalid text UTF-8", "data:text/plain;base64,/w==", translate.ClassTranslation},
			{"invalid text base64", "data:text/plain;base64,!", translate.ClassTranslation},
		} {
			t.Run(from+"/"+tc.name, func(t *testing.T) {
				part := map[string]any{"type": "input_file", "file_data": tc.data, "filename": "a.pdf"}
				if from == translate.FormatChatCompletions {
					part = map[string]any{"type": "file", "file": map[string]any{"file_data": tc.data, "filename": "a.pdf"}}
				}
				parts, err := json.Marshal([]any{part})
				if err != nil {
					t.Fatal(err)
				}
				body, convErr := translate.ConvertRequest(translate.FormatMessages, "upstream", from, mediaRequest(from, "user", string(parts)), translate.NewRequestState())
				if convErr == nil || convErr.Class != tc.class || body != nil {
					t.Fatalf("body=%s error=%v, want %s", body, convErr, tc.class)
				}
			})
		}
	}
}

func TestMessagesTextDocumentRoundTrip(t *testing.T) {
	input := mediaRequest(translate.FormatMessages, "user", `[{"type":"text","text":"before"},{"type":"document","title":"a.txt","source":{"type":"text","media_type":"text/plain","data":"hello\n世界\n"}},{"type":"text","text":"after"}]`)
	for _, intermediate := range []string{translate.FormatChatCompletions, translate.FormatResponses} {
		t.Run(intermediate, func(t *testing.T) {
			state := translate.NewRequestState()
			state.Options.LossPolicy = translate.LossPolicyStrict
			converted, err := translate.ConvertRequest(intermediate, "upstream", translate.FormatMessages, input, state)
			if err != nil {
				t.Fatal(err)
			}
			restored, err := translate.ConvertRequest(translate.FormatMessages, "upstream", intermediate, converted, state)
			if err != nil {
				t.Fatal(err)
			}
			want := object(t, input)["messages"].([]any)[0].(map[string]any)["content"]
			if got := mediaContent(t, translate.FormatMessages, restored); !reflect.DeepEqual(got, want) {
				t.Fatalf("restored=%v, want %v", got, want)
			}
			if len(state.Diagnostics()) != 0 {
				t.Fatalf("unexpected losses: %v", state.Diagnostics())
			}
		})
	}
}

func TestMessagesToolResultsRejectDocuments(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, kind := range []string{"chat-tool", "function_call_output", "custom_tool_call_output"} {
			for _, file := range []string{
				`{"type":"input_file","file_data":"data:application/pdf;base64,JVBERg=="}`,
				`{"type":"input_file","file_data":"data:text/plain;base64,aGVsbG8="}`,
				`{"type":"input_file","file_url":"https://example.test/a.pdf"}`,
			} {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", kind, stream, file), func(t *testing.T) {
					from := translate.FormatResponses
					parts := `[{"type":"text","text":"before"},{"type":"input_image","image_url":"https://example.test/a.png"},` + file + `,{"type":"text","text":"after"}]`
					payload := fmt.Sprintf(`{"stream":%t,"input":[{"type":%q,"call_id":"c1","output":%s}]}`, stream, kind, parts)
					if kind == "chat-tool" {
						from = translate.FormatChatCompletions
						var part map[string]any
						json.Unmarshal([]byte(file), &part)
						delete(part, "type")
						chatFile, _ := json.Marshal(map[string]any{"type": "file", "file": part})
						parts = `[{"type":"text","text":"before"},{"type":"image_url","image_url":{"url":"https://example.test/a.png"}},` + string(chatFile) + `,{"type":"text","text":"after"}]`
						payload = fmt.Sprintf(`{"stream":%t,"messages":[{"role":"tool","tool_call_id":"c1","content":%s}]}`, stream, parts)
					}
					body, err := translate.ConvertRequest(translate.FormatMessages, "upstream", from, []byte(payload), translate.NewRequestState())
					if err == nil || err.Class != translate.ClassUnsupported || body != nil {
						t.Fatalf("body=%s, error=%v", body, err)
					}
				})
			}
		}
	}
}

func TestNullDocumentMetadataHasNoLoss(t *testing.T) {
	input := mediaRequest(translate.FormatMessages, "user", `[{"type":"document","context":null,"citations":null,"source":{"type":"base64","media_type":"application/pdf","data":"JVBERg=="}}]`)
	for _, to := range []string{translate.FormatChatCompletions, translate.FormatResponses} {
		for _, policy := range []translate.LossPolicy{translate.LossPolicyAllow, translate.LossPolicySafe, translate.LossPolicyStrict} {
			state := translate.NewRequestState()
			state.Options.LossPolicy = policy
			if _, err := translate.ConvertRequest(to, "upstream", translate.FormatMessages, input, state); err != nil {
				t.Fatalf("%s/%s: %v", to, policy, err)
			}
			if ds := state.Diagnostics(); len(ds) != 0 {
				t.Fatalf("%s/%s: %v", to, policy, ds)
			}
		}
	}
}
