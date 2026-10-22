package route_test

import (
	"fmt"
	"log"
	"slices"

	"github.com/dillonzq/apix"
	"github.com/dillonzq/apix/protocol/anthropic"
	"github.com/dillonzq/apix/protocol/openai"
	"github.com/dillonzq/apix/route"
)

// Endpoints speaking the client's protocol come first, whatever their
// priority; conversion fallbacks follow.
func ExampleRouter_Route() {
	reg, err := apix.NewRegistry(slices.Concat(openai.Codecs(), anthropic.Codecs())...)
	if err != nil {
		log.Fatal(err)
	}
	r, err := route.New(reg, []route.Endpoint{
		{ID: "openai", Protocol: openai.ChatCompletions, Priority: 0, Models: map[string]string{"smart": "gpt-4.1"}},
		{ID: "anthropic", Protocol: anthropic.Messages, Priority: 1, Models: map[string]string{"smart": "claude-sonnet-4-5"}},
		{ID: "azure", Protocol: openai.Responses, Priority: 2, Models: map[string]string{"*": ""}},
	})
	if err != nil {
		log.Fatal(err)
	}
	info, aerr := reg.Inspect(anthropic.Messages, apix.Payload{Body: []byte(`{"model":"smart","max_tokens":64,"messages":[]}`)})
	if aerr != nil {
		log.Fatal(aerr)
	}
	candidates, aerr := r.Route(info)
	if aerr != nil {
		log.Fatal(aerr)
	}
	for _, c := range candidates {
		fmt.Println(c.Endpoint.ID, c.UpstreamModel, c.Native)
	}
	// Output:
	// anthropic claude-sonnet-4-5 true
	// openai gpt-4.1 false
	// azure smart false
}
