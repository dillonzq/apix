package usage

import (
	"errors"
	"strings"
	"testing"

	"github.com/dillonzq/apix/sse"
)

func TestTrackerFrameLimit(test *testing.T) {
	wire := "data: {\"usage\":{\"prompt_tokens\":3}}\n\n" + strings.Repeat("data: x\n", 20)
	for _, step := range []int{1, 7, len(wire)} {
		tracker := New(ChatCompletions, 64)
		for offset := 0; offset < len(wire); offset += step {
			tracker.Feed([]byte(wire[offset:min(offset+step, len(wire))]))
		}
		tracker.End()
		tracker.Feed([]byte("data: {\"usage\":{\"prompt_tokens\":99}}\n\n"))
		observed, ok := tracker.Usage()
		if !ok || observed.InputTokens != 3 || !errors.Is(tracker.Err(), sse.ErrFrameTooLarge) {
			test.Fatalf("step=%d usage=%+v err=%v", step, observed, tracker.Err())
		}
		if len(tracker.partial) != 0 || len(tracker.data) != 0 {
			test.Fatal("failed tracker retained partial payload")
		}
	}
}
