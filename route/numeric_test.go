package route_test

import (
	"math"
	"slices"
	"testing"

	"github.com/dillonzq/apix/protocol/anthropic"
	"github.com/dillonzq/apix/route"
)

func TestExtremePriorityOrder(test *testing.T) {
	router := newRouter(test, []route.Endpoint{
		{ID: "highest", Protocol: anthropic.Messages, Priority: math.MaxInt},
		{ID: "lowest", Protocol: anthropic.Messages, Priority: math.MinInt},
		{ID: "zero", Protocol: anthropic.Messages},
	})
	candidates, err := router.Route(messagesRequest("m"))
	if err != nil || !slices.Equal(ids(candidates), []string{"lowest", "zero", "highest"}) {
		test.Fatalf("extreme priorities: %v %v", ids(candidates), err)
	}
}

func TestOverflowingWeights(test *testing.T) {
	router := newRouter(test, []route.Endpoint{
		{ID: "first", Protocol: anthropic.Messages, Weight: math.MaxInt},
		{ID: "second", Protocol: anthropic.Messages, Weight: math.MaxInt},
		{ID: "small", Protocol: anthropic.Messages, Weight: 1},
	})
	firstPicks := 0
	for range 1000 {
		candidates, err := router.Route(messagesRequest("m"))
		if err != nil {
			test.Fatal(err)
		}
		if candidates[0].Endpoint.ID == "first" {
			firstPicks++
		}
		ordered := ids(candidates)
		slices.Sort(ordered)
		if !slices.Equal(ordered, []string{"first", "second", "small"}) {
			test.Fatalf("candidate lost or duplicated: %v", ordered)
		}
	}
	if firstPicks < 400 || firstPicks > 600 {
		test.Fatalf("overflow handling distorted equal weights: %d/1000", firstPicks)
	}
}
