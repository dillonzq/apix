package gateway_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/dillonzq/apix"
	"github.com/dillonzq/apix/gateway"
	"github.com/dillonzq/apix/protocol/anthropic"
	"github.com/dillonzq/apix/sse"
)

type uncheckedFrameCodec struct{ apix.Codec }

func (codec uncheckedFrameCodec) NewExchange(client apix.Protocol, options apix.ExchangeOptions) (apix.Exchange, error) {
	options.MaxStreamFrameBytes = -1
	return codec.Codec.NewExchange(client, options)
}

func TestGatewayFrameLimitCommitBoundary(test *testing.T) {
	for _, unchecked := range []bool{false, true} {
		for _, committed := range []bool{false, true} {
			test.Run(map[bool]string{false: "builtin", true: "unchecked-codec"}[unchecked]+"/"+map[bool]string{false: "before", true: "after"}[committed], func(test *testing.T) {
				wire := strings.Repeat("x", 129)
				if committed {
					wire = messagesStart + wire
				}
				primary := newUpstream(test, respond(200, "text/event-stream", wire))
				fallback := newUpstream(test, respond(200, "text/event-stream", chatStream))
				reg := registry(test)
				if unchecked {
					codecs := make([]apix.Codec, 0, len(reg.Protocols()))
					for _, protocol := range reg.Protocols() {
						codec, _ := reg.Codec(protocol)
						codecs = append(codecs, uncheckedFrameCodec{codec})
					}
					var err error
					reg, err = apix.NewRegistry(codecs...)
					if err != nil {
						test.Fatal(err)
					}
				}
				instance, err := gateway.New(gateway.Config{Registry: reg, MaxStreamFrameBytes: 128, Upstreams: []gateway.Upstream{messagesUpstream(primary), chatUpstream(fallback)}})
				if err != nil {
					test.Fatal(err)
				}
				response, failure := do(test, instance, clientStream)
				if failure != nil {
					test.Fatal(failure)
				}
				defer response.Body.Close()
				if committed {
					body, err := io.ReadAll(response.Body)
					if string(body) != messagesStart || !errors.Is(err, sse.ErrFrameTooLarge) || fallback.hits() != 0 {
						test.Fatalf("committed limit failure: body=%q error=%v fallback=%d", body, err, fallback.hits())
					}
				} else {
					readAll(test, response)
					if response.Endpoint != "chat" || len(response.Attempts) != 2 || !errors.Is(response.Attempts[0].Err, sse.ErrFrameTooLarge) {
						test.Fatalf("pre-commit limit did not fail over: %+v", response.Attempts)
					}
				}
			})
		}
	}
}

func TestGatewayFrameLimitOverridesAndLongStreams(test *testing.T) {
	for _, maximum := range []int{512, -1} {
		wire := ":" + strings.Repeat("x", 256) + "\n\n" + messagesStart + strings.Repeat(":heartbeat\n\n", 100) + "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
		primary := newUpstream(test, respond(200, "text/event-stream", wire))
		upstream := messagesUpstream(primary)
		upstream.Options.MaxStreamFrameBytes = maximum
		instance := newGateway(test, gateway.Config{MaxStreamFrameBytes: 128, Upstreams: []gateway.Upstream{upstream}})
		response, err := instance.Do(context.Background(), gateway.Request{Protocol: anthropic.Messages, Payload: apix.Payload{Body: []byte(clientStream)}})
		if err != nil {
			test.Fatal(err)
		}
		if body := readAll(test, response); !strings.Contains(string(body), "message_stop") {
			test.Fatal("valid long stream was limited by its total size")
		}
	}
}
