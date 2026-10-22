package apix_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/dillonzq/apix"
)

// fakeCodec is a minimal codec for registry behaviour. It accepts the client
// protocols listed in accepts.
type fakeCodec struct {
	id      apix.Protocol
	kind    apix.Kind
	accepts []apix.Protocol
	headers map[string]string
}

func (c fakeCodec) Spec() apix.ProtocolSpec {
	return apix.ProtocolSpec{ID: c.id, Kind: c.kind, Headers: c.headers}
}
func (c fakeCodec) Inspect(apix.Payload) (apix.RequestInfo, *apix.Error) {
	return apix.RequestInfo{Model: "m"}, nil
}
func (c fakeCodec) EncodeError(err *apix.Error) apix.Payload {
	return apix.Payload{Body: []byte(string(c.id) + ":" + err.Message)}
}
func (c fakeCodec) Accepts(client apix.Protocol) bool {
	return client == c.id || slices.Contains(c.accepts, client)
}
func (c fakeCodec) NewExchange(client apix.Protocol, _ apix.ExchangeOptions) (apix.Exchange, *apix.Error) {
	return nil, nil
}

var (
	textA  = fakeCodec{id: "a.text", kind: apix.KindLLM, accepts: []apix.Protocol{"b.text", "x.image"}}
	textB  = fakeCodec{id: "b.text", kind: apix.KindLLM}
	imageX = fakeCodec{id: "x.image", kind: apix.KindImage, accepts: []apix.Protocol{"a.text"}}
)

func TestNewRegistryValidation(t *testing.T) {
	for name, codecs := range map[string][]apix.Codec{
		"nil":       {nil},
		"no id":     {fakeCodec{kind: apix.KindLLM}},
		"no kind":   {fakeCodec{id: "a"}},
		"duplicate": {textA, fakeCodec{id: "a.text", kind: apix.KindAudio}},
	} {
		if _, err := apix.NewRegistry(codecs...); err == nil {
			t.Fatalf("%s: registered", name)
		}
	}
	reg, err := apix.NewRegistry(textB, imageX, textA)
	if err != nil {
		t.Fatal(err)
	}
	if got := reg.Protocols(); !slices.Equal(got, []apix.Protocol{"a.text", "b.text", "x.image"}) {
		t.Fatalf("protocols %v", got)
	}
	if _, ok := reg.Codec("missing"); ok {
		t.Fatal("found unregistered codec")
	}
}

func TestCanConvert(t *testing.T) {
	reg, _ := apix.NewRegistry(textA, textB, imageX)
	for _, tc := range []struct {
		client, upstream apix.Protocol
		want             bool
	}{
		{"a.text", "a.text", true},   // same protocol
		{"b.text", "a.text", true},   // accepted
		{"a.text", "b.text", false},  // b accepts nothing else
		{"x.image", "a.text", false}, // accepted but a different Kind
		{"a.text", "x.image", false}, // a different Kind
		{"missing", "a.text", false},
		{"a.text", "missing", false},
	} {
		if got := reg.CanConvert(tc.client, tc.upstream); got != tc.want {
			t.Fatalf("%s -> %s = %v", tc.client, tc.upstream, got)
		}
	}
	if _, err := reg.NewExchange("a.text", "b.text", apix.ExchangeOptions{}); err == nil || err.Class != apix.ClassUnsupported {
		t.Fatalf("unsupported exchange: %v", err)
	}
	if _, err := reg.NewExchange("missing", "a.text", apix.ExchangeOptions{}); err == nil || !strings.Contains(err.Message, "missing") {
		t.Fatalf("unregistered client: %v", err)
	}
}

func TestRegistrySpecHeadersAreIsolated(t *testing.T) {
	c := fakeCodec{id: "a.text", kind: apix.KindLLM, headers: map[string]string{"Version": "original"}}
	reg, err := apix.NewRegistry(c)
	if err != nil {
		t.Fatal(err)
	}
	c.headers["Version"] = "changed by codec owner"
	spec, _ := reg.Spec(c.id)
	if spec.Headers["Version"] != "original" {
		t.Fatalf("registered headers changed: %v", spec.Headers)
	}
	spec.Headers["Version"] = "changed by caller"
	spec.Headers["Extra"] = "unexpected"
	again, _ := reg.Spec(c.id)
	if again.Headers["Version"] != "original" || len(again.Headers) != 1 {
		t.Fatalf("Spec exposes shared headers: %v", again.Headers)
	}
}

// recordingCodec captures the options its exchanges are created with.
type recordingCodec struct {
	fakeCodec
	got *apix.ExchangeOptions
}

func (c recordingCodec) NewExchange(_ apix.Protocol, opts apix.ExchangeOptions) (apix.Exchange, *apix.Error) {
	*c.got = opts
	return nil, nil
}

func TestExchangeOutput(t *testing.T) {
	var got apix.ExchangeOptions
	upstream := recordingCodec{fakeCodec: textA, got: &got}
	reg, err := apix.NewRegistry(upstream, textB, imageX)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.NewExchange("b.text", "a.text", apix.ExchangeOptions{}); err != nil || got.Output != "b.text" {
		t.Fatalf("default output %q: %v", got.Output, err)
	}
	if _, err := reg.NewExchange("b.text", "a.text", apix.ExchangeOptions{Output: "a.text"}); err != nil || got.Output != "a.text" {
		t.Fatalf("explicit output %q: %v", got.Output, err)
	}
	for _, tc := range []struct {
		name                     string
		client, upstream, output apix.Protocol
	}{
		{"unregistered", "b.text", "a.text", "missing"},
		{"other kind", "b.text", "a.text", "x.image"}, // a accepts x.image, but Kinds differ
		{"no response path", "b.text", "b.text", "a.text"},
	} {
		if _, err := reg.NewExchange(tc.client, tc.upstream, apix.ExchangeOptions{Output: tc.output}); err == nil || err.Class != apix.ClassUnsupported {
			t.Fatalf("%s: %v", tc.name, err)
		}
	}
}

func TestInspectUsesRegisteredIdentity(t *testing.T) {
	reg, _ := apix.NewRegistry(imageX)
	info, err := reg.Inspect("x.image", apix.Payload{})
	if err != nil || info.Protocol != "x.image" || info.Kind != apix.KindImage || info.Model != "m" {
		t.Fatalf("%+v %v", info, err)
	}
	if _, err := reg.Inspect("missing", apix.Payload{}); err == nil {
		t.Fatal("inspected an unregistered protocol")
	}
}

func TestEncodeError(t *testing.T) {
	reg, _ := apix.NewRegistry(textA)
	if got := string(reg.EncodeError("a.text", apix.Translation("bad")).Body); got != "a.text:bad" {
		t.Fatalf("codec body %q", got)
	}
	var doc struct{ Error struct{ Message string } }
	body := reg.EncodeError("missing", apix.Translation("bad \x00 \"x\""))
	if err := json.Unmarshal(body.Body, &doc); err != nil || doc.Error.Message != "bad \x00 \"x\"" {
		t.Fatalf("fallback body %s: %v", body.Body, err)
	}
}

func TestHTTPStatus(t *testing.T) {
	for _, tc := range []struct {
		err  *apix.Error
		want int
	}{
		{nil, http.StatusOK},
		{apix.FromStatus(503, "down"), 503},
		{apix.FromStatus(429, "slow"), 429},
		{apix.FromNetwork(nil), http.StatusGatewayTimeout},
		{apix.UpstreamFallback("x"), http.StatusBadGateway},
		{apix.Translation("x"), http.StatusBadRequest},
		{apix.Unsupported("x"), http.StatusBadRequest},
		{&apix.Error{Class: apix.ClassInvalidModel}, http.StatusNotFound},
		{&apix.Error{Class: apix.ClassAuth}, http.StatusUnauthorized},
		{&apix.Error{Class: apix.ClassBilling}, http.StatusPaymentRequired},
	} {
		if got := apix.HTTPStatus(tc.err); got != tc.want {
			t.Fatalf("%v: %d", tc.err, got)
		}
	}
}

func TestRequestInfoNeeds(t *testing.T) {
	info := apix.RequestInfo{Features: []string{apix.FeatureStream}}
	if !info.Needs(apix.FeatureStream) || info.Needs(apix.FeatureTools) {
		t.Fatal("Needs")
	}
}
