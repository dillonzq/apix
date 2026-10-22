<!-- Keep the summary short; reviewers read the diff and the snapshot changes first. -->

## Summary

<!-- What changes, and why. Reference the issue: "Fixes #123". -->

## Type of change

- [ ] Bug fix — wrong, dropped, or invalid output; crash; stream failure
- [ ] New capability — new representable field, tool shape, or mapping
- [ ] Refactor — internal change with no wire-output difference
- [ ] Documentation, tests, or pipeline only

## Affected conversion paths

<!-- Tick every direction whose output, diagnostics, or errors change. -->

- [ ] `openai.chat-completions` → `openai.responses`
- [ ] `openai.chat-completions` → `anthropic.messages`
- [ ] `openai.responses` → `openai.chat-completions`
- [ ] `openai.responses` → `anthropic.messages`
- [ ] `anthropic.messages` → `openai.chat-completions`
- [ ] `anthropic.messages` → `openai.responses`
- [ ] Same-protocol model rewrite or passthrough
- [ ] `openai.ResponsesOptions{NormalizeTools: true}` (native Responses tool normalization)
- [ ] Streaming only (`StreamConverter`, `sse.Decoder`)
- [ ] Routing or gateway behavior (`route`, `gateway`)
- [ ] A new protocol or kind
- [ ] No output change

## Checklist

- [ ] `make check` passes (formatting, vet, module/dependency checks, build, and race tests)
- [ ] `make lint` passes; workflow changes also pass `make workflow-lint`
- [ ] No new production or test dependencies (`make deps`)
- [ ] Output-producing decodes handle their error; documented best-effort probes use `shared.DecodeProbe` or `shared.StringField`
- [ ] New losses are reported as `Diagnostic` values instead of being dropped silently, and their behavior under `LossPolicySafe` / `LossPolicyStrict` is intended
- [ ] Snapshot changes (`internal/llm/translate/testdata/conversion/`) are reviewed line by line and explained below
- [ ] Streaming changes cover full input, bytewise input, missing final separator, and EOF without termination (or N/A)
- [ ] Tool identity registration and rollback behavior are covered by a test (or N/A)
- [ ] `README.md`/`README.zh-CN.md` and `docs/*.md`/`docs/*.zh-CN.md` are updated where behavior or limits changed
- [ ] Any `//nolint` directive states a reason, and `.golangci.yml` is unchanged unless the pull request explains the change

## Snapshot and protocol notes

<!-- Quote the interesting snapshot lines. Link the upstream specification for field-level changes. -->

## Reviewer notes

<!-- Trade-offs, follow-ups, and anything you deliberately left out. -->
