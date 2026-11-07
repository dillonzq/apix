package google_test

import (
	"fmt"

	"github.com/dillonzq/apix"
	"github.com/dillonzq/apix/protocol/google"
	"github.com/dillonzq/apix/protocol/openai"
)

func ExampleGenerateContentCodec() {
	reg, err := apix.NewRegistry(openai.ChatCompletionsCodec(), google.GenerateContentCodec())
	if err != nil {
		panic(err)
	}
	ex, aerr := reg.NewExchange(google.GenerateContent, openai.ChatCompletions, apix.ExchangeOptions{})
	if aerr != nil {
		panic(aerr)
	}
	stream := true
	req := apix.Payload{Body: []byte(`{"contents":[{"role":"user","parts":[{"text":"Hello"}]}]}`), Metadata: apix.RequestMetadata{Model: "public-model", Stream: &stream}}
	info, aerr := reg.Inspect(google.GenerateContent, req)
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
