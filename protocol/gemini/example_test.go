package gemini_test

import (
	"fmt"

	"github.com/dillonzq/apix"
	"github.com/dillonzq/apix/protocol/gemini"
	"github.com/dillonzq/apix/protocol/openai"
)

func ExampleGenerateContentCodec() {
	reg, err := apix.NewRegistry(openai.ChatCompletionsCodec(), gemini.GenerateContentCodec())
	if err != nil {
		panic(err)
	}
	ex, aerr := reg.NewExchange(gemini.GenerateContent, openai.ChatCompletions, apix.ExchangeOptions{})
	if aerr != nil {
		panic(aerr)
	}
	stream := true
	req := apix.Payload{Body: []byte(`{"contents":[{"role":"user","parts":[{"text":"Hello"}]}]}`), Metadata: apix.RequestMetadata{Model: "public-model", Stream: &stream}}
	info, aerr := reg.Inspect(gemini.GenerateContent, req)
	if aerr != nil {
		panic(aerr)
	}
	converted, aerr := ex.ConvertRequest(req, "upstream-model")
	if aerr != nil {
		panic(aerr)
	}
	fmt.Println(info.Model, info.Stream)
	fmt.Println(string(converted.Body))
	// Output:
	// public-model true
	// {"messages":[{"content":[{"text":"Hello","type":"text"}],"role":"user"}],"model":"upstream-model","stream":true}
}
