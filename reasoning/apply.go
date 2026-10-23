package reasoning

import (
	"github.com/dillonzq/apix"
	"github.com/dillonzq/apix/internal/llm/ids"
	rs "github.com/dillonzq/apix/internal/llm/reasoning"
	"github.com/dillonzq/apix/protocol/gemini"
)

// StripSupersededReasoning removes the client's own reasoning control from the
// source payload when a recognized suffix supersedes it. Only the (target,
// source) pairs whose conversion derives the target control from that field
// are touched, because only those can reject a value the suffix already
// replaced. Everything else keeps its body untouched. An apix.Exchange with
// ExchangeOptions.Reasoning does this itself.
func StripSupersededReasoning(target, source apix.Protocol, body []byte) []byte {
	to, ok := ids.Format(target)
	from, ok2 := ids.Format(source)
	if !ok || !ok2 {
		return body
	}
	return rs.Strip(to, from, body)
}

// Options select how ApplySuffixWith renders a suffix for an upstream. Use
// the values configured on the upstream's exchange options.
type Options struct {
	// MessagesManual renders Messages suffixes as enabled thinking with
	// budget_tokens, for Claude models that predate adaptive thinking. Set it
	// when the exchange uses anthropic.ThinkingManual.
	MessagesManual bool
	// GeminiMode matches gemini.GenerateContentOptions.ThinkingMode: budget
	// mode sends levels as thinkingBudget, level mode sends positive budgets
	// as thinkingLevel. Empty sends numeric suffixes as budgets and levels as
	// levels. Gemini has no level above high, so xhigh and max become high.
	GeminiMode gemini.ThinkingMode
}

// ApplySuffix is ApplySuffixWith with zero Options: Messages suffixes use
// adaptive thinking, matching the default Messages exchange.
func ApplySuffix(target apix.Protocol, body []byte, suffix Suffix) ([]byte, error) {
	return ApplySuffixWith(target, body, suffix, Options{})
}

// ApplySuffixWith rewrites the reasoning control of an upstream body that has
// already been converted, so the requested model's suffix takes priority over
// the control the client sent. It runs only for a recognized suffix value:
// absent or unrecognized suffixes, and protocols without a reasoning control,
// leave the body byte-identical.
func ApplySuffixWith(target apix.Protocol, body []byte, suffix Suffix, opts Options) ([]byte, error) {
	r := suffix.Reasoning()
	format, ok := ids.Format(target)
	if r == nil || !ok {
		return body, nil
	}
	out, _, err := rs.Apply(format, body, *r, rs.Options{MessagesManual: opts.MessagesManual, GeminiMode: string(opts.GeminiMode)})
	if err != nil {
		return nil, err
	}
	return out, nil
}
