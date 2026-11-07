package gateway_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/dillonzq/apix"
	"github.com/dillonzq/apix/gateway"
)

func TestDefaultClientRejectsCrossOriginRedirect(test *testing.T) {
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		for _, custom := range []bool{false, true} {
			test.Run(http.StatusText(status)+map[bool]string{false: "/default-auth", true: "/custom-auth"}[custom], func(test *testing.T) {
				target := newUpstream(test, respond(200, "application/json", messagesReply))
				origin := newUpstream(test, func(writer http.ResponseWriter, request *http.Request) {
					http.Redirect(writer, request, strings.Replace(target.URL, "127.0.0.1", "localhost", 1), status)
				})
				upstream := messagesUpstream(origin)
				if custom {
					upstream.Auth = &apix.AuthScheme{Header: "X-Private-Key"}
				}
				instance := newGateway(test, gateway.Config{Upstreams: []gateway.Upstream{upstream}})
				if response, err := do(test, instance, clientRequest); err == nil {
					_ = response.Body.Close()
					test.Fatal("cross-origin redirect succeeded")
				}
				if target.hits() != 0 {
					test.Fatal("redirect target received a request carrying credentials")
				}
			})
		}
	}
}

func TestDefaultClientAllowsSameOriginRedirect(test *testing.T) {
	origin := newUpstream(test, func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/redirected" {
			http.Redirect(writer, request, "/redirected", http.StatusTemporaryRedirect)
			return
		}
		if request.Header.Get("X-Api-Key") != "k-msg" || request.Method != http.MethodPost {
			test.Error("same-origin redirect lost auth or changed method")
		}
		respond(200, "application/json", messagesReply)(writer, request)
	})
	instance := newGateway(test, gateway.Config{Upstreams: []gateway.Upstream{messagesUpstream(origin)}})
	response, err := do(test, instance, clientRequest)
	if err != nil {
		test.Fatal(err)
	}
	readAll(test, response)
	if origin.hits() != 2 {
		test.Fatal("same-origin redirect was not followed")
	}
}
