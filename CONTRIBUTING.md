# Contributing

Thanks for helping improve `apix`. This library converts requests, non-streaming responses, and streams between provider API protocols, and routes requests across providers. It uses only the Go standard library. The core packages (`apix`, `protocol/*`, `route`, `sse`, `reasoning`) perform no network I/O; only the optional `gateway` package sends requests. Wire output is the product, so behavior changes are reviewed as protocol changes rather than as ordinary refactors.

By taking part you agree to keep discussion technical and respectful, and to license your contribution under the [MIT License](LICENSE).

## Ways to contribute

- **Bug reports** with a minimal, redacted JSON or SSE sample and the affected direction. These are the most valuable reports; see [the bug template](.github/ISSUE_TEMPLATE/bug_report.yml).
- **Conversion fixes** for fields that are dropped, mis-mapped, or re-encoded incorrectly.
- **New representable behavior** when a protocol gains a field with a real counterpart in another protocol. Open an issue first if the mapping is debatable.
- **New protocols and kinds** through the `apix.Codec` interface, following [adding protocols and kinds](docs/extending.md). Open an issue first to agree on the protocol ID and the direct conversion paths.
- **Tests, snapshots, and documentation** for behavior that is already implemented, including the wrong-JSON-type cases that a buggy or hostile upstream can send.
- **Pipeline improvements** that make failures easier to diagnose.

## Out of scope

The following deliberately stay outside this repository. Changes that pull them in are usually rejected:

- Network I/O outside `gateway`. The core decides; it never sends.
- Provider account policy beyond protocol defaults: billing, model capability checks, and model-name validation.
- Lossy conveniences that silently invent data the upstream never sent.
- A shared intermediate representation. Conversion paths stay direct; a codec lists the client protocols it accepts.
- Third-party dependencies. CI checks the production and test dependency graphs for packages outside the standard library and this module (see [Design rules](#design-rules)).

## Requirements

| Tool | Version | Notes |
| --- | --- | --- |
| Go | 1.26.10 or newer | Pinned by `go.mod`; `actions/setup-go` installs the same version in CI. |
| golangci-lint | v2.14.0 | Only needed for the `lint` job. |
| git | any recent | — |
| GNU Make and Bash | any recent | Shared local/CI check commands. On Windows, use WSL for `make`; native Go commands also work. |

```sh
git clone https://github.com/dillonzq/apix.git
cd apix
make check
```

Install the pinned linter with the Go version from `go.mod` or newer. Package managers may provide a different version; check `golangci-lint version` before comparing results with CI:

```sh
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.14.0
```

## Run the same checks as CI

[The CI workflow](.github/workflows/ci.yml) runs on pull requests, pushes to `main`, manual dispatch, and a weekly schedule. The release workflow calls the same checks after validating the tag. [The Makefile](Makefile) is the shared entry point for local and Linux CI checks; `make help` lists common commands.

```sh
make check # Fast pre-push checks, including race tests; no extra tools needed
make ci    # Full Linux CI checks; requires the pinned golangci-lint
```

Both Make and CI export `GOFLAGS=-mod=readonly`, so builds cannot silently add missing requirements, and `GOWORK=off`, so local workspaces cannot change the module under review. Coverage is informational and has no threshold. CI caches Go downloads and build results using `go.mod` as the dependency key because this module has no `go.sum`. The pinned actionlint and govulncheck versions live in the Makefile; their tool dependencies are downloaded outside this module and do not change `go.mod`.

| CI job | Local check |
| --- | --- |
| `lint` | `make fmt-check vet tidy-check deps lint workflow-lint` |
| `test` | `make build coverage` |
| `fuzz smoke` | `make fuzz` (override duration with `FUZZTIME=2m`) |
| `vulnerability scan` | `make vuln` (uses the live vulnerability database) |
| `test (macos-latest)` / `test (windows-latest)` | `bash scripts/check-dependencies.sh`, `go build ./...`, and `go test -shuffle=on -count=1 ./...` on that OS |
| `CI passed` | Aggregate gate: every validation job, including both OS tests, must succeed |

Linux runs race tests and coverage; macOS and Windows check their native production/test dependency graphs, builds, and tests using the same Go version from `go.mod`. GNU Make and Bash are only required for the shared command targets. On Windows without WSL, use the equivalent Go commands above for native tests, `go vet ./...`, and `go mod tidy -diff`; full `make ci` requires an environment that supports Go's race detector and a C compiler.

### Diagnose a failed check

- Formatting: run `make fmt` and review the diff.
- Module files: run `go mod tidy`, inspect the diff, then retry `make tidy-check deps`. The dependency check includes test imports and fails if package loading itself fails.
- Tests: download the `test-output` artifact for `test.log` and `coverage.out`, or use `make coverage` locally. The Actions summary includes package results and total coverage. Generated logs and coverage are ignored by Git.
- Shuffled tests: copy the printed seed and reproduce with `go test -race -shuffle=<seed> -count=1 ./...`. Use `-run '<TestName>'` for a focused regression.
- Fuzzing: download `fuzz-failures`, preserve the input under `testdata/fuzz/<TargetName>/` of the target's package (`internal/llm/translate` or `protocol/gemini`), then run `go test ./<package> -run '<TargetName>/<hash>'` to replay it. `make fuzz FUZZTIME=2m` runs all three targets for longer.
- Workflow changes: run `make workflow-lint`. Changes to check commands must update the Makefile and this guide together.
- Vulnerabilities: reproduce with `make vuln`, inspect the affected call path, and update the Go patch version or code as appropriate. Follow [SECURITY.md](SECURITY.md) for private reports.

The weekly run catches new vulnerability database findings even when there are no code changes. Workflow concurrency cancels superseded runs on the same ref; separate release tags have separate groups.

## Project layout

| Path | Contents |
| --- | --- |
| Root package (`apix`) | Protocol-neutral API: `Kind`, `Protocol`, `Codec`, `Exchange`, `Registry`, errors, diagnostics. |
| `protocol/openai`, `protocol/anthropic`, `protocol/gemini` | Vendor codecs: protocol IDs, specs, and vendor exchange options. |
| `route` | Endpoint selection, the native-first strategy, health tracking. |
| `gateway` | Optional HTTP executor and handler with failover. |
| `sse` | SSE framing and decoding. |
| `reasoning` | Optional reasoning controls and model-name suffixes. Not part of the default conversion path. |
| `internal/llm/ids` | Public protocol IDs, internal format names, and the mapping between them. |
| `internal/llm/codec` | The `apix.Codec` implementations for LLM protocols: the protocol table, routing inspection, state capture, error envelopes. |
| `internal/llm/translate` | The LLM conversion engine entry points and execution state. |
| `internal/llm/adapter/chatcompletions`, `messages`, `responses` | Per-protocol request, response, and stream conversion. |
| `internal/llm/adapter/gemini` | Every GenerateContent path, in both directions. |
| `internal/llm/adapter/interactions` | Native Interactions response and stream validation. |
| `internal/llm/reasoning` | Reasoning level/budget tables and override rendering per protocol. |
| `internal/llm/adapter/shared` | Shared LLM pieces: decode types, content and media mapping, tool identity, diagnostics, usage. |
| `internal/errclass` | Short aliases of `apix.Error` and its constructors; classification, status mapping, and redaction live in the root `errors.go`. |
| `internal/llm/translate/testdata/conversion/{request,response,stream}` | The reviewed conversion snapshots, named `<from>_to_<to>.json` with internal format names. |

[Code structure and maintenance](docs/architecture.md) describes dispatch rules and state contracts in more detail.

## Design rules

1. **No dependencies.** The `lint` job uses `scripts/check-dependencies.sh` to resolve production and test package graphs and checks module ownership. It rejects any external module, including similarly named sibling modules, and propagates package-loading errors. Discuss an import in an issue before proposing it.
2. **Public packages are the API.** Everything under `internal/` may change at any time; do not re-export internals unless a caller genuinely needs the value. Codecs are registered explicitly; never add `init()` registration or package-level registries.
3. **Keep all twelve LLM cross-protocol directions working.** Chat Completions, Responses, Messages, and GenerateContent each convert to the other three. A mapping added to one direction usually needs its counterpart; same-protocol requests only rewrite the model.
4. **Requests dispatch by target protocol; responses and streams dispatch by upstream protocol.** An exchange from client X to upstream Y is implemented by Y's adapter; any path with GenerateContent at either end belongs to the Gemini adapter. Do not collapse the direct paths into a shared intermediate representation.
5. **State is per execution.** An `Exchange` carries tool identities, options, and diagnostics for one request. Registries, codecs, routers, and gateways are safe for concurrent use. No package-level mutable state or caches, and no goroutines that outlive a call; long-lived state such as the affinity cache belongs to a router instance.
6. **The core decides; the gateway executes.** Conversion and routing never perform I/O. Failover happens only before the commit point; after it, failures surface to the caller. Report what happened through `*apix.Error` and diagnostics instead of guessing.
7. **Never emit secrets.** Error messages are logged by callers; wrap your own text with `apix.Redact`. Code under `internal/` calls `errclass.Redact`. The gateway never forwards client credentials.
8. **Decode errors are handled, not discarded.** Output-producing decodes must check the error. A genuine best-effort probe goes through `shared.DecodeProbe`, whose doc comment states the contract: the target can be partially modified on a shape mismatch, so the helper provides no validation or rollback guarantee. Use it only for non-authoritative inspection, never to rebuild required output from possibly partial values. `shared.StringField` performs a checked read of a single string field and reports `ok=false` so callers can stop instead of substituting an empty string.

## Changing conversion behavior

Work through this list; reviewers will check it against the diff.

1. **Identify the direction and the entry point.** Confirm whether the change belongs to a request, an ordinary response, or a stream, and whether it affects native normalization (`openai.ResponsesOptions{NormalizeTools: true}`) rather than cross-protocol conversion.
2. **Decide representability before writing code.** If the target protocol cannot carry a field faithfully, record a [`Diagnostic`](README.md#exchange-options) with a stable code and path instead of dropping it silently. `Diagnostic` carries `Code`, `Path`, `Message`, `Semantic`, `From`, and `To`: `Semantic` marks a loss that changes request semantics, which is what `LossPolicySafe` rejects.
3. **Respect the loss policy.** The policy decides request outcomes only: `LossPolicySafe` rejects semantic request losses, `LossPolicyStrict` rejects every reported request loss, and responses and streams report losses without being rejected because of them. They still fail on malformed data, upstream failures, and termination errors.
4. **Preserve what the current path preserves.** Paths that use `json.RawMessage` keep unknown fields and exact numbers; do not add a full re-encode through modeled types.
5. **Keep tool identities intact.** Register declarations and history references through the existing transaction helpers so a failed conversion leaves the registry unchanged.
6. **Streams have their own contract.** Preserve event order, emit `[DONE]` only for Chat output, keep `sequence_number` monotonic for synthesized Responses events, validate termination in `Finish`, and never discard events that were produced before an error.
7. **Update snapshots deliberately.** `go test ./internal/llm/translate -run 'Test(Protocol|GeminiProtocol)Golden' -update` rewrites the 48 snapshots under `internal/llm/translate/testdata/conversion/`. Read every changed line and explain it in the pull request. Never regenerate snapshots to make a red test green; if you cannot explain a diff, that is the bug.
8. **Update the documentation.** Behavior, limits, and direction matrices live in [LLM protocol conversion](docs/protocols/llm.md); module responsibilities live in [code structure](docs/architecture.md); routing and failover rules live in [routing](docs/routing.md). Each document has a `.zh-CN.md` counterpart that must be updated in the same pull request, as does the README pair when the public API or installation changes.

## Tests

- Test protocol behavior through the engine entry points in `internal/llm/translate` (`ConvertRequest`, `ConvertResponse`, `NewStreamConverter`, `NormalizeResponsesRequest`), which the public `Exchange` is pinned to byte for byte. Reach into adapters only for parsers, collectors, and other state components.
- Test routing and gateway behavior through `route` and `gateway` with `httptest` upstreams; never call real providers from tests.
- Prefer table-driven tests with a name per case, and add one regression case per fixed bug.
- Cover the shapes a hostile or buggy upstream can send, not only the happy path: a field carrying the wrong JSON type (a non-string `arguments`, a `citations` object where an array is expected, a query that is a number), an integer beyond 2^53, and a literal outside the `float64` range such as `1e400`. These are the cases where a discarded decode error becomes wrong output, and `DecodeProbe` must never be the reason a value reaches the caller as an empty string.
- Every stream change should cover: whole input, bytewise input, a missing final separator, and EOF without a terminal event.
- Fuzz targets must not depend on randomness or state beyond the input bytes they are handed. If fuzzing finds a crash, keep the finding: add the input to the target package's `testdata/fuzz/<TargetName>/` and a focused regression test alongside it.
- Benchmarks (`Benchmark*` in `internal/llm/adapter/...`) are informative, not gated. Run them with `go test -run '^$' -bench . ./internal/llm/adapter/...` when you touch hot paths.
- Tests must pass with `-race`. Do not add shared state to make a test convenient.

## Formatting, lint, and comments

- `.gitattributes` keeps text files in LF on all platforms, including Windows. Golden snapshots are compared byte for byte; keep these checkout rules when adding fixtures.
- Formatting is `gofmt` only; CI fails on any unformatted file. `gofumpt`-style rewrites are welcome in a dedicated pull request but are not enforced.
- `.golangci.yml` pins the checks. Deliberate deviations are documented there:
  - `errcheck` has no global exclusions. Output-producing decodes handle their error, and documented best-effort probes call `shared.DecodeProbe`, so a new bare `json.Unmarshal` fails lint.
  - `gocritic` keeps its default checks and adds the `diagnostic` tag; `ifElseChain` is disabled explicitly because the converters keep if/else chains for mixed predicates.
  - `staticcheck` keeps correctness (`SA*`) and simplicity (`S1*`) checks and disables style (`ST*`) and quickfix (`QF*`) classes.
- Suppress a finding inline only with a reason: `//nolint:nilerr // an undecodable body carries no protocol error envelope`. `nolintlint` requires a specific linter, an explanation, and no unused directive, so prefer a comment over widening the configuration.
- Actions are pinned to full commit SHAs with version comments; Dependabot proposes reviewed updates. Changing `.golangci.yml` or the pinned versions in CI is a reviewed change: state what it catches and why the exclusions are safe.
- Write comments for the reason, not the mechanics, and keep exported identifiers documented; reviewers read them.
- Keep the tone of the existing code: complete sentences, protocol terms in backticks, no invented guarantees.

## Commits and branches

This repository follows [Conventional Commits](https://www.conventionalcommits.org/): lowercase type, optional scope, imperative subject, no trailing period.

```
fix: reject document media in Messages tool results
feat(route): add native-first routing with health tracking
refactor: move the LLM engine under internal/llm
docs: refresh READMEs and add project logo
```

| Prefix | Used for |
| --- | --- |
| `feat` | New representable behavior or public API |
| `fix` | Wrong, dropped, or invalid output; crashes; stream failures |
| `refactor` | Internal restructuring with no wire-output change |
| `docs` | README, `docs/`, and comment-only changes |
| `test` | Tests, snapshots, and fuzzing |
| `ci` | Workflows, lint configuration, dependency updates |
| `design` | Logo, branding, and other visual assets, as used in the existing history |
| `chore` | Anything else that is not user-visible |

Name branches `<type>/<short-topic>`, for example `feat/responses-shell-tool` or `fix/messages-document-tool-results`. Keep one logical change per pull request; a refactor plus a behavior change is two.

## Pull requests

1. Open an issue first for anything larger than a focused fix, so the mapping question is settled before the implementation.
2. Fill in the [pull request template](.github/pull_request_template.md), including the affected directions and every snapshot change.
3. Make sure `CI passed` is green, including all underlying validation jobs. Coverage is informational; a drop needs a reason, not a threshold.
4. Expect review on protocol semantics, not style: how the field maps in both directions, what happens under each loss policy, and what the snapshots now say.
5. Do not rewrite published history on `main`. Force-push only your own feature branch.

## Releases (maintainers)

Versioning is [Semantic Versioning](https://semver.org/), tagged `vX.Y.Z` from `main`. Before 1.0 the public API is still settling: a conversion change that alters wire output can land in a minor bump, but it still belongs in the release notes because callers may depend on the exact bytes. After 1.0 the same change requires a major bump. The module path has no `/vN` suffix, so only `v0` and `v1` tags are accepted; a `v2` tag would need a `/v2` module path first.

The release workflow gates the GitHub Release, not the module version: once a tag is pushed, the Go toolchain can already resolve it. Cut releases only from commits that are on `main` and already green.

1. Confirm `CI passed` is green on the exact `main` commit you intend to tag.
2. Land the release preparation as a normal pull request: the version in both READMEs (the `go get` snippet and the release badge), any "unreleased behavior" notes that the release makes obsolete, and the version/location tables in `docs/`.
3. Merge to `main`, then confirm the commit to tag: `git fetch origin && git log --oneline -1 origin/main`.
4. Tag and push:

   ```sh
   git tag -a vX.Y.Z origin/main -m "Release vX.Y.Z"
   git push origin vX.Y.Z
   ```

5. [`.github/workflows/release.yml`](.github/workflows/release.yml) first validates the version against the module path and checks that the commit is an ancestor of `origin/main`. Only then does it run the full CI workflow on the tagged commit and create the GitHub Release with generated notes when every check passes. Tags with a suffix such as `v0.1.0-rc.1` are published as pre-releases.
6. Re-running the release job is safe: an existing published release is left untouched, while an existing draft is published only after validation and CI pass. Draft titles, notes, and assets are preserved; prerelease status follows the tag suffix. Never move a published tag; correct the notes with `gh release edit` instead.
7. Prereleases are for feedback; promote them by tagging the final version once the notes are accurate.
8. Configure branch protection or a ruleset to require `CI passed` (Settings → Branches or Rules → Rulesets). This stable check fails on failed, cancelled, or skipped validation jobs, including matrix jobs. When adding a validation job, add it to the aggregate job's `needs` list. Retire any old required check names only after the new check has run successfully on a pull request; these repository settings are maintained separately from workflow files.

## Reporting bugs and security issues

Use [the issue templates](.github/ISSUE_TEMPLATE) for bugs and feature requests. A useful bug report contains the library version, the Go version, the exact direction (or `openai.ResponsesOptions{NormalizeTools: true}`), the entry point (`Exchange`, `route`, or `gateway`), a minimal redacted sample, and the expected versus actual bytes.

Do not report security vulnerabilities in public issues. See [SECURITY.md](SECURITY.md) and use [private vulnerability reporting](https://github.com/dillonzq/apix/security/advisories/new). Reports about leaked credentials in error messages, diagnostic paths, or stream output are treated as security issues.
