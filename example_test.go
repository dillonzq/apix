package apix_test

import (
	"fmt"
	"log"
	"slices"

	"github.com/dillonzq/apix"
	"github.com/dillonzq/apix/protocol/anthropic"
	"github.com/dillonzq/apix/protocol/openai"
)

// An Anthropic Messages client talks to an OpenAI Chat Completions upstream.
// The caller sends the converted request with its own HTTP client.
func Example() {
	reg, err := apix.NewRegistry(slices.Concat(openai.Codecs(), anthropic.Codecs())...)
	if err != nil {
		log.Fatal(err)
	}
	ex, aerr := reg.NewExchange(anthropic.Messages, openai.ChatCompletions, apix.ExchangeOptions{})
	if aerr != nil {
		log.Fatal(aerr)
	}
	client := []byte(`{"model":"claude","max_tokens":64,"messages":[{"role":"user","content":"Hello"}]}`)
	upstream, aerr := ex.ConvertRequest(apix.Payload{Body: client}, "gpt-4.1")
	if aerr != nil {
		log.Fatal(aerr)
	}
	fmt.Println(string(upstream.Body))

	reply := []byte(`{"id":"chatcmpl-1","object":"chat.completion","model":"gpt-4.1","choices":[{"index":0,"message":{"role":"assistant","content":"Hi!"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`)
	out, aerr := ex.ConvertResponse(200, apix.Payload{Body: reply})
	if aerr != nil {
		log.Fatal(aerr)
	}
	fmt.Println(string(out.Body))
	// Output:
	// {"model":"gpt-4.1","messages":[{"role":"user","content":"Hello"}],"max_tokens":64}
	// {"content":[{"text":"Hi!","type":"text"}],"id":"chatcmpl-1","model":"gpt-4.1","role":"assistant","stop_reason":"end_turn","type":"message","usage":{"input_tokens":3,"output_tokens":2}}
}
