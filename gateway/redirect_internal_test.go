package gateway

import (
	"net/http"
	"net/url"
	"testing"
)

func TestRedirectOriginPolicy(test *testing.T) {
	origin, err := url.Parse("https://provider.example/v1/messages")
	if err != nil {
		test.Fatal(err)
	}
	via := []*http.Request{{URL: origin}}
	for _, fixture := range []struct {
		target  string
		allowed bool
	}{
		{"https://provider.example/redirect", true},
		{"https://PROVIDER.example:443/redirect", true},
		{"https://provider.example:444/redirect", false},
		{"http://provider.example/redirect", false},
		{"https://other.example/redirect", false},
		{"https://provider.example.attacker.example/redirect", false},
	} {
		target, err := url.Parse(fixture.target)
		if err != nil {
			test.Fatal(err)
		}
		if err := checkRedirect(&http.Request{URL: target}, via); (err == nil) != fixture.allowed {
			test.Fatalf("%s: %v", fixture.target, err)
		}
	}
	if err := checkRedirect(&http.Request{URL: origin}, make([]*http.Request, 10)); err == nil {
		test.Fatal("redirect count was not bounded")
	}
}
