# Prompt Templates — Plugin Wiring Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the render engine reachable from a Bifrost request — a plugin that resolves a stored prompt, gathers variables, renders the stored template and the client's own messages, and merges model parameters.

**Architecture:** A `store/` interface layer keeps the module free of Bifrost's persistence types. A `Plugin` type implements `schemas.LLMPlugin` plus the HTTP transport hooks: the transport hook captures variables from the header and body, and `PreLLMHook` does resolution, rendering and parameter merging. The plugin layer owns the one control the engine structurally cannot provide — a wall-clock deadline — because `Execute` takes no `context.Context` and only the caller owns the goroutine.

**Tech Stack:** Go 1.26.6, `github.com/maximhq/bifrost/core`, `github.com/nikolalohinski/gonja/v2` v2.9.0.

**Spec:** `docs/superpowers/specs/2026-08-25-prompt-templates-design.md`

**Predecessors:** `2026-08-25-prompt-templates-engine.md` and `2026-08-25-prompt-templates-authored.md` — both complete, 217 tests passing. This plan consumes `engine.Untrusted`, `engine.Authored`, `engine.Cache`, `engine.Budget`, `engine.AsMissingVariables`, `partials.Set`, and `render.Merge`/`render.DeclaredNames`.

**Repo:** `arbi-ai/bifrost-prompt-templates` at `/Users/cameron/projects/neuroscale/oss/bifrost-prompt-templates`. This plan adds the **first** dependency on `github.com/maximhq/bifrost/core`, in the new `plugin/` package only. `engine/`, `partials/` and `render/` must stay Bifrost-free so they remain unit-testable without the gateway.

## Global Constraints

- Go pinned to **1.26.6**; `github.com/maximhq/bifrost/core` at the version the fork uses.
- `engine/`, `partials/`, `render/` must not import `github.com/maximhq/bifrost/*`. Add a guard test mirroring `engine/banned_test.go`.
- Untrusted rendering never fails a request. Authored rendering fails it with a typed error.
- Every fallback is metered and logged, with the error **truncated** — a failed render can produce a multi-megabyte error string, and logging it verbatim relocates the DoS to the log pipeline.

**Verified Bifrost API shapes** (checked against the fork before writing, not assumed):

| Symbol | Shape |
|---|---|
| `schemas.LLMPlugin` | `BasePlugin` + `PreRequestHook`, `PreLLMHook`, `PostLLMHook` |
| `schemas.BifrostRequest` | struct of per-type pointers; `ChatRequest`, `ResponsesRequest` are the two this plan touches |
| `schemas.ChatMessage` | `{Name *string; Role; Content *ChatMessageContent}` + embedded `*ChatToolMessage`, `*ChatAssistantMessage` |
| `schemas.ChatMessageContent` | `{ContentStr *string; ContentBlocks []ChatContentBlock}` — exactly one is set |
| `schemas.ChatContentBlock` | `{Type; Text *string; Refusal *string; ImageURLStruct *ChatInputImage; InputAudio; File; ...}` |
| `schemas.ChatInputImage` | `{URL string; FileID *string; Detail *string}` |
| `schemas.ResponsesMessage` | `{Role *ResponsesMessageRoleType; Content *ResponsesMessageContent; ...}` + **embedded** `*ResponsesToolMessage`, `*ResponsesReasoning` |
| `schemas.ResponsesMessageContent` | `{ContentStr *string; ContentBlocks []ResponsesMessageContentBlock}` |
| `schemas.ResponsesMessageContentBlock` | Named: `Type`, `Text *string`, `FileID`, `Signature`, `EncryptedContent`, `Audio`, `CacheControl`, `Citations`, `PromptCacheBreakpoint`. **Embedded pointers**: `*ResponsesInputMessageContentBlockImage`, `*ResponsesInputMessageContentBlockFile` |
| `schemas.HTTPRequest` / `HTTPResponse` | `{Method, Path, Headers, Query, Body []byte, PathParams}` / `{StatusCode, Headers, Body}` |

Note `ChatMessageContent` is a two-field struct where exactly one field is populated, **not** an interface — walking it means checking both.

`ChatContentBlock` also carries `CacheControl`, `Citations` (`{Enabled *bool}`), `PromptCacheBreakpoint` and `CachePoint`. None hold templatable text, so passing them through is correct — recorded here so the next reader does not re-derive it.

### Embedded pointers: reading a promoted field can panic

**This is the single most likely way to break Task 3.** `ChatMessage` and `ResponsesMessageContentBlock` promote fields through *embedded pointers*, and merely **reading** such a field when the embedding is nil is a nil dereference — no method call needed:

```go
var m schemas.ChatMessage          // a plain user message
_ = m.Content                      // fine: named field
_ = m.Refusal                      // PANICS: promoted through a nil *ChatAssistantMessage
_ = m.ToolCalls                    // PANICS
_ = m.ToolCallID                   // PANICS: nil *ChatToolMessage

var b schemas.ResponsesMessageContentBlock  // a plain input text block
_ = b.Text                         // fine: named field
_ = b.ImageURL                     // PANICS: nil *ResponsesInputMessageContentBlockImage
```

The trap is that **the two paths differ**. On a Chat block, `Refusal *string` is a *named* field and `cb.Refusal` is safe. On a Responses block there is no top-level `Refusal` — it resolves through an embedded pointer and panics. Writing the Responses walk "by analogy" with Chat therefore panics on an input text block carrying only `Type` and `Text`, which is the most common shape a client sends.

**Rule: nil-check the embedding, never the promoted field.**

```go
if b.ResponsesInputMessageContentBlockImage != nil {
    // b.ImageURL is now safe to read
}
```

And **"passed through untouched" means not referenced at all** — not "read and then ignored".

---

### Task 1: Store interfaces and the Bifrost-free guard

**Files:**
- Create: `store/store.go`
- Test: `store/store_test.go`, `engine/nobifrost_test.go`

**Interfaces:**
- Produces:
  - `type PromptVersion struct{ PromptID string; Version int; TenantID string; Messages []Message; ModelParams map[string]any; Variables map[string]any }`
  - `type Message struct{ Role string; Content string }`
  - `type PromptStore interface{ ResolveVersion(ctx, promptID string, version int) (*PromptVersion, error) }`
  - `type PartialStore interface{ PartialsFor(ctx, tenantID string) ([]partials.Partial, error) }`

`ResolveVersion` takes the **requested** version and returns the **resolved** one, so the caller keys the cache on what it actually got. Version `0` means "latest".

- [ ] **Step 1: Write the failing tests**

`store/store_test.go` asserts the interfaces are satisfiable by a hand-written fake (compile-time assertion plus a round trip). `engine/nobifrost_test.go` walks `engine/`, `partials/`, `render/` and `store/` and fails if any file imports `github.com/maximhq/bifrost/`:

```go
// The engine must stay unit-testable without a gateway. Only plugin/ may
// depend on Bifrost.
func TestCorePackagesDoNotImportBifrost(t *testing.T) {
	for _, pkg := range []string{"engine", "partials", "render", "store"} {
		// parse each .go file, inspect import specs, collect offenders
	}
}
```

Use `go/parser` and inspect `file.Imports`, mirroring `engine/banned_test.go` — a substring grep flags its own documentation, which that test learned the hard way.

- [ ] **Step 2: Run to verify it fails** — `go test ./store/ -v` (package missing).

- [ ] **Step 3: Implement** `store/store.go` with the types above. No behaviour, only interfaces and plain structs.

- [ ] **Step 4: Run to verify it passes** — `go test ./... -v`.

- [ ] **Step 5: Commit** — `feat(store): prompt and partial store interfaces`

---

### Task 2: Variable extraction from header and body

**Files:**
- Create: `plugin/variables.go`
- Test: `plugin/variables_test.go`

**Interfaces:**
- Produces:
  - `func VariablesFromHeader(raw string) (map[string]any, error)`
  - `func VariablesFromBody(body []byte) (map[string]any, error)`
  - `const VariablesHeader = "x-bf-prompt-variables"`
  - `const VariablesBodyField = "variables"`

- [ ] **Step 1: Write the failing test**

Cover: a JSON object header parses; nested objects and arrays survive; malformed JSON returns an error rather than panicking; an absent header yields nil with no error; the body field is extracted without disturbing other fields; a body with no `variables` yields nil; and a body that is not an object yields nil rather than an error (a malformed body is the transport's problem, not this plugin's).

- [ ] **Step 2: Run to verify it fails.**

- [ ] **Step 3: Implement.** Parse with `encoding/json` into `map[string]any`. Do **not** use `UseNumber` — the engine's `CheckIterables` reflects over values and gonja arithmetic expects native numeric kinds.

- [ ] **Step 4: Run to verify it passes.**

- [ ] **Step 5: Commit** — `feat(plugin): variable extraction from header and body`

---

### Task 3: Message walking

The renderers take a string; a Bifrost request holds structured messages. This task is the adapter, and it is where the spec's rendering scope is enforced.

**Files:**
- Create: `plugin/walk.go`
- Test: `plugin/walk_test.go`

**Interfaces:**
- Produces:
  - `func RenderChatMessages(msgs []schemas.ChatMessage, r TextRenderer) ([]schemas.ChatMessage, []Outcome)`
  - `func RenderResponsesMessages(msgs []schemas.ResponsesMessage, r TextRenderer) ([]schemas.ResponsesMessage, []Outcome)`
  - `type TextRenderer func(s string) (string, error)`

- [ ] **Step 1: Write the failing test**

The scope rules, each a test:

- `ContentStr` is rendered.
- Every `ContentBlocks` entry with `Type == text` has its `Text` rendered (`Text` is a named field on both block types — safe to read).
- Chat: `ImageURLStruct.URL` **is** rendered — templated signed URLs are a real use case. `ImageURLStruct` is a named field, so nil-check it directly.
- Responses: the image URL is `ResponsesInputMessageContentBlockImage.ImageURL *string`, reached through an **embedded pointer**. Render it for parity with Chat, guarded by `if b.ResponsesInputMessageContentBlockImage != nil`.
- Audio and file blocks are passed through untouched — binary, nothing to template.
- `Refusal` is passed through untouched, meaning **never read**. It is model output, and on the Responses path reading it panics.
- Tool calls and tool arguments are **not** walked: out of scope for v1 per the spec. This is a deliberate gap, not an omission — see the note after these rules.
- A `nil` `Content` is a no-op, not a panic. Both fields nil is a no-op.
- **A plain user message — no assistant fields, no tool fields — walks without panicking.** This is the regression test for the embedded-pointer trap; write it first.
- A Responses input text block carrying only `Type` and `Text` walks without panicking.
- The walk renders into **copies** and returns them; it does not mutate in place. See Task 4.

**On the tool-call gap.** The spec puts tool/function JSON out of scope for v1, and `ChatAssistantMessage.ToolCalls` / `ResponsesToolMessage.Arguments` are the only text-bearing fields excluded. Two consequences to document rather than discover: a stored prompt version *can* contain assistant few-shot turns with tool calls — the existing `prompts` plugin's `completion_result` envelope produces exactly that — so an author writing `{{ }}` inside tool arguments gets silence; and the walk must not so much as *read* `msg.ToolCalls` without nil-checking `msg.ChatAssistantMessage` first.

- [ ] **Step 2: Run to verify it fails.**

- [ ] **Step 3: Implement.** `ChatMessageContent` has exactly one of `ContentStr`/`ContentBlocks` populated, so check both and act on whichever is set. Guard every embedded-pointer access as above. Return rendered copies rather than mutating the caller's slice.

- [ ] **Step 4: Run to verify it passes.**

- [ ] **Step 5: Commit** — `feat(plugin): walk chat and responses messages`

---

### Task 4: The render deadline

The engine cannot bound wall-clock time: `exec.Template.Execute` takes no `context.Context`, so nothing inside a render can be interrupted. Only the caller — which owns the goroutine — can impose a deadline, and this is that caller.

**Files:**
- Create: `plugin/deadline.go`
- Test: `plugin/deadline_test.go`

**Interfaces:**
- Produces:
  - `func WithDeadline[T any](d time.Duration, fn func() (T, error)) (T, error)`
  - `var ErrRenderDeadline = errors.New("render deadline exceeded")`

**Per REQUEST, not per render.** A per-render deadline lets N messages each finish just under it, for N × deadline total. Compute the deadline once in `PreLLMHook` and pass the remaining time down, mirroring `engine.Budget`, which the plan already shares per-request for exactly this reason.

**The abandoned goroutine must not hold anything the request path still uses.** This is why Task 3 renders into copies rather than mutating in place: with in-place mutation, a timed-out render keeps rewriting the very messages the request has already carried on to the provider. Verified with the race detector against both tasks exactly as originally specified:

```
WARNING: DATA RACE
  Read at 0x00c000014980 by main goroutine
  Previous write at 0x00c000014980 by goroutine 8   (the abandoned render)
```

Neither task's own tests would catch it — Task 4 tests `WithDeadline` with a self-contained `fn`, Task 3 tests the walk with no deadline, and each passes `-race` alone. Commit rendered results to the request **only on success**, which also makes a deadline all-or-nothing instead of leaving a half-rendered request in flight.

The render closure must likewise never capture the fasthttp `RequestCtx` — same hazard class, since that context is recycled.

- [ ] **Step 1: Write the failing test**

Cover: a fast function returns its value; a slow function returns `ErrRenderDeadline` within roughly the deadline; the slow goroutine finishing later does not panic on a closed channel or corrupt the result; a panicking function surfaces as an error rather than killing the process; and — the T2 regression — **a timed-out render that shares a slice with the caller does not race**, asserted under `-race` with the walk and the deadline composed as Task 7 composes them.

- [ ] **Step 2: Run to verify it fails.**

- [ ] **Step 3: Implement.** Run `fn` in a goroutine, select on a buffered result channel and a timer.

**Document the limitation honestly in the code:** this **abandons** a slow render, it does not stop it. The goroutine keeps running until `fn` returns, holding its memory. That is why the engine's structural guards — restricted control structures, the expression-size bound, loop depth, iterable caps — are the primary controls and this is a backstop.

The result channel **must** be buffered (cap 1) so the late send never blocks; an unbuffered channel would leak the goroutine permanently rather than letting it exit and be collected. Beyond its own stack and allocations the abandoned goroutine leaks nothing — *provided* it shares no references with the request path, which is the constraint above.

- [ ] **Step 4: Run to verify it passes** — with `-race`.

- [ ] **Step 5: Commit** — `feat(plugin): wall-clock render deadline`

---

### Task 5: Model parameter merge

**Files:**
- Create: `plugin/params.go`
- Test: `plugin/params_test.go`

**Interfaces:**
- Produces:
  - `func ApplyChatParams(version *store.PromptVersion, req *schemas.BifrostChatRequest) error`
  - `func ApplyResponsesParams(version *store.PromptVersion, req *schemas.BifrostResponsesRequest) error`

Ported from `plugins/prompts/main.go`: version params are defaults, request params win. The marshal-merge-unmarshal round trip is safe because every `ChatParameters` field is an `omitempty` pointer, so request zero-values cannot clobber version defaults.

**CORRECTED after measurement.** This task originally said to extend `knownSyntheticChatParamKeys` to the Responses path. That is wrong, and implementing it would have introduced a silent data-loss bug. Probed against the real types:

| Input | Chat | Responses |
|---|---|---|
| `reasoning_effort` | consumed → `.Reasoning.Effort`; re-marshals as `reasoning` | **not** consumed; `.Reasoning` stays nil |
| `reasoning_display` | consumed → `.Reasoning.Display` | not consumed; no `Display` field exists |
| flat + nested together | `UnmarshalJSON` **errors** | flat ignored, nested wins |

`ResponsesParameters` has no custom `UnmarshalJSON`, so it promotes nothing. Skipping the key there would drop the setting entirely rather than prevent a duplicate — the skip list is only correct where the key is actually consumed.

Three distinct inherited bugs follow, and the fix addresses them together:

1. **Responses files the key wrongly** — it reaches the provider as a bogus top-level `reasoning_effort` while `.Reasoning` is nil.
2. **`reasoning_display` is a third shorthand the list omits** — on Chat it is promoted *and* duplicated into `ExtraParams`, reaching the provider twice.
3. **The conflict case discards everything** — a version using the shorthand plus a client sending nested `reasoning` makes the Chat unmarshal error, and the original logs a warning and returns early, dropping *every* version param, not just the reasoning one. This is the ordinary case, not an edge one.

**Fix:** fold each source's flat shorthands into that source's own nested `reasoning` object *before* merging, then delete the flat key. Order matters — normalising after the merge cannot tell a version shorthand from a request nested value. The key is then absent from the merged map, so no skip list is needed and all three modes go away at once. A single source carrying both forms stays an error: there is no precedence rule between two values written by the same author.

Note the fold sets differ per path: Chat folds `effort`/`max_tokens`/`display`, Responses folds `effort`/`max_tokens` only, so `reasoning_display` correctly remains an extra param on the Responses path rather than being invented away.

- [ ] **Step 1: Write the failing test**

Cover: version params apply when the request omits them; request params win; unrecognised keys land in `ExtraParams`; request `ExtraParams` outrank version defaults; empty version params are a no-op. Then one test per bug above, plus the mirror case (request shorthand beats version nested) and a malformed version param surfacing as an error rather than a silent drop.

- [ ] **Step 2–5:** as usual. Commit — `feat(plugin): model parameter merge, resolving reasoning shorthands before merge`

---

### Task 6: The transport hook

**Files:**
- Create: `plugin/transport.go`
- Test: `plugin/transport_test.go`

**Interfaces:**
- Produces: `HTTPTransportPreAuthHook`, `HTTPTransportPreHook`, `HTTPTransportPostHook`, `HTTPTransportStreamChunkHook` on `*Plugin`, plus context keys.

- [ ] **Step 1: Write the failing test**

Three transport facts, each verified against the fork and each needing a test:

- **`variables` must be stripped from `ExtraParams`.** `extractExtraParams` (`handlers/inference.go`) sweeps every field absent from `chatParamsKnownFields` / `responsesParamsKnownFields` into `Params.ExtraParams`, and `variables` is in neither list. Left in place, a caller sending `x-bf-passthrough-extra-params: true` gets it merged verbatim into the outgoing provider body and a 400 from upstream.
- **Integration routes drop `variables` at parse — but the transport hook runs first.** `integrations/router.go` parses with a plain `sonic.Unmarshal` into the integration's own request type, so an unknown top-level field is gone before the *handler* sees it. **Corrected:** `TransportInterceptorMiddleware` wraps `next(ctx)`, so `HTTPTransportPreHook` observes the **raw body before that parse**. Body variables therefore do work on `/openai/*`, just not *guaranteed* — see the body-copy caveat below. The header is the only **guaranteed** transport there. Test both, and document that the guarantee, not the capability, is what differs.
- **The body copy is skipped far more often than "large bodies" suggests.** `fasthttpToHTTPRequest` (`handlers/middlewares.go`) skips it when content length exceeds the threshold **or is negative**. Unknown content length covers chunked transfer *and* anything that passed through streaming decompression, which deletes `Content-Length` — so an ordinary gzipped request loses its body in the transport hook regardless of size. The threshold itself is **10 MB** (`DefaultLargePayloadRequestThresholdBytes`, `core/schemas/bifrost.go`). Read `variables` from the **parsed request** in `PreLLMHook`; the transport hook's body read is an optimisation that finds nothing *routinely*, not exceptionally.

- [ ] **Step 2–5:** as usual. Commit — `feat(plugin): transport hook and variable capture`

---

### Task 7: The plugin and PreLLMHook

**Files:**
- Create: `plugin/plugin.go`
- Test: `plugin/plugin_test.go`

**Interfaces:**
- Produces:
  - `const PluginName = "prompt-templates"`
  - `type Config struct{ Limits; DeadlineMS int; CacheSize int; RenderClientMessages bool }`
  - `func Init(ctx context.Context, ps store.PromptStore, pt store.PartialStore, cfg *Config, logger schemas.Logger) (*Plugin, error)`
  - `GetName`, `PreRequestHook`, `PreLLMHook`, `PostLLMHook`, `Cleanup`

- [ ] **Step 1: Write the failing test**

`PreLLMHook` behaviour, in order:

1. No prompt header and no variables → request untouched, no error.
2. A resolved prompt renders its stored messages through `engine.Authored` and **prepends** them to the client's input.
3. Client messages render through `engine.Untrusted` with `render.DeclaredNames` as the allowlist.
4. A missing variable in the **stored template** short-circuits with a 400 naming the variables, via `engine.AsMissingVariables`. `LLMPluginShortCircuit.Error` is a `*schemas.BifrostError` (`core/schemas/plugin_native.go`) carrying `StatusCode *int` and `Error *ErrorField` — not a raw HTTP body. Set `AllowFallbacks` false: a missing variable is not retryable against another provider.
5. A missing variable in a **client message** leaves that message verbatim and does not fail the request.
6. Variable precedence is version defaults → header → body.
7. The cache key carries tenant, resolved version, partial fingerprint and per-message source hash.
8. One `engine.Budget` is shared across every message of a request.
9. A render exceeding the deadline fails the request rather than hanging.
10. Fallbacks increment a counter and log at debug with the error **truncated** to a bounded length.

- [ ] **Step 2: Run to verify it fails.**

- [ ] **Step 3: Implement.**

Order inside `PreLLMHook`: resolve → build partial set → merge variables → apply params → **capture the client's message slice** → render stored messages (authored) → render the captured client messages (untrusted) → assemble `stored ++ client` → return.

**Capture before prepending.** Reading `req.ChatRequest.Input` *after* prepending sends the stored template's output through `engine.Untrusted` for a second pass. That is wasted work, but it is also semantically wrong: the guarantee that variable values are not re-parsed holds *within* one render, not across two — a value containing template syntax is inert on pass 1 and live on pass 2. The untrusted set limits the blast radius (a parse error just falls back verbatim), but `{% raw %}` output and any literal `{{` a stored template deliberately emits would be mangled.

**The `prompts` coexistence check belongs in `Init`, not per-request.** Both plugins write `BifrostContextKeySelectedPromptID`, and a context-key guard only works if `prompts` runs first — which tie-breaks on registration order (`lib/config.go`), and `prompts` has no reciprocal guard. `Init` returns an error naming both when `prompts` is also enabled. A hard boot failure is correct: the alternative is a silently doubled prompt on every request, discovered in production.

- [ ] **Step 4: Run to verify it passes** — `go test ./... -race`.

- [ ] **Step 5: Commit** — `feat(plugin): prompt templates plugin`

---

### Task 8: Fork wiring

**Files (in the fork, `/Users/cameron/projects/neuroscale/oss/bifrost`):**
- Modify: `transports/bifrost-http/server/plugins.go`, `transports/bifrost-http/lib/config.go`, `transports/config.schema.json`
- Create: a store adapter backed by the framework config store

- [ ] **Step 1: Write the failing test** — a transport-level integration test issuing `POST /v1/chat/completions` with `x-bf-prompt-id` and a body `variables` object, asserting the rendered prompt reaches the provider mock.

- [ ] **Step 2–5:** wire `case prompttemplates.PluginName` in `plugins.go`, add to the builtin list, add the config block, and implement the adapter mapping `configstoreTables.TablePrompt`/`TablePromptVersion` onto `store.PromptVersion`.

**Set the plugin order explicitly, and pin it with a test.** Observed builtin placements: `prompts` 2, `governance` 4, `semanticcache` 7. A plugin registered without order info defaults to `PluginPlacementPostBuiltin` and would run **after** semantic cache — which would then key on the *unrendered* template, so two requests with different `variables` but the same stored prompt collide and return each other's cached responses. Call `SetPluginOrderInfo(..., builtinPlacement, schemas.Ptr(2))` to take the `prompts` slot. Governance at 4 running after us is correct: prepended messages must be visible to it.

**Keep the fork diff small.** All logic lives in the module; this task is imports, a case, a config block and an adapter.

Commit — `feat(transports): wire the prompt-templates plugin`

---

## Out of scope

- `/v1/prompts/{id}/render`, the `prompt_partials` table and partial CRUD — Plan 4.
- UI and the `.so` build — Plan 5.
- `extends` via literal-graph cycle detection — still deferred.

## Self-Review

**Spec coverage.** Store interfaces → Task 1. Header/body variables → Tasks 2, 6. Rendering scope → Task 3. Deadline → Task 4. Param merge and the synthetic-key fix → Task 5. `ExtraParams` strip, integration-route gap, large-body caveat → Task 6. Precedence, strict-vs-lenient, cache keying, shared budget, fallback metering, `prompts` coexistence → Task 7. Fork wiring → Task 8.

**Type consistency.** `store.PromptVersion` is defined in Task 1 and consumed in Tasks 5, 7, 8. `TextRenderer` is defined in Task 3 and consumed in Task 7. `Config` is defined in Task 7 and consumed in Task 8. Task 3's renderers return copies, which Task 7 commits only on success.

**Verified, not assumed.** Every Bifrost type in the table above was read from the fork before writing. Two shapes are easy to get wrong and are called out where used: `ChatMessageContent` is a two-field struct with exactly one field populated rather than an interface, and `ChatContentBlock` carries the image URL under `ImageURLStruct.URL` rather than a plain string field.

**Known risk carried forward.** Task 4's deadline abandons rather than stops. Every plan so far has restated this; it stays true, and it is why the engine's structural guards do the real work.
