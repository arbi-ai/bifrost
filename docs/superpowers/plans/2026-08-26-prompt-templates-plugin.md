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
| `schemas.ResponsesMessage` | `{Role *ResponsesMessageRoleType; Content *ResponsesMessageContent; ...}` |
| `schemas.ResponsesMessageContent` | `{ContentStr *string; ContentBlocks []ResponsesMessageContentBlock}` |
| `schemas.HTTPRequest` / `HTTPResponse` | `{Method, Path, Headers, Query, Body []byte, PathParams}` / `{StatusCode, Headers, Body}` |

Note `ChatMessageContent` is a two-field struct where exactly one field is populated, **not** an interface — walking it means checking both.

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
  - `func RenderChatMessages(msgs []schemas.ChatMessage, r TextRenderer) []Outcome`
  - `func RenderResponsesMessages(msgs []schemas.ResponsesMessage, r TextRenderer) []Outcome`
  - `type TextRenderer func(s string) (string, error)`

- [ ] **Step 1: Write the failing test**

The scope rules, each a test:

- `ContentStr` is rendered.
- Every `ContentBlocks` entry with `Type == text` has its `Text` rendered.
- `ImageURLStruct.URL` **is** rendered — templated signed URLs are a real use case.
- `InputAudio` and `File` blocks are passed through untouched (binary; nothing to template).
- `Refusal` is passed through untouched — it is model output, not author or user input.
- A `nil` `Content` is a no-op, not a panic.
- Both `ContentStr` and `ContentBlocks` nil is a no-op.
- Messages are mutated **in place**; the returned outcomes report per-message results.
- The Responses path covers `ContentStr` and `ContentBlocks` equivalently.

- [ ] **Step 2: Run to verify it fails.**

- [ ] **Step 3: Implement.** A straightforward walk. `ChatMessageContent` has exactly one of `ContentStr`/`ContentBlocks` populated, so check both and act on whichever is set.

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

- [ ] **Step 1: Write the failing test**

Cover: a fast function returns its value; a slow function returns `ErrRenderDeadline` within roughly the deadline; the slow goroutine finishing later does not panic on a closed channel or corrupt the result; and a panicking function surfaces as an error rather than killing the process.

- [ ] **Step 2: Run to verify it fails.**

- [ ] **Step 3: Implement.** Run `fn` in a goroutine, select on a buffered result channel and a timer.

**Document the limitation honestly in the code:** this **abandons** a slow render, it does not stop it. The goroutine keeps running until `fn` returns, holding its memory. That is why the engine's structural guards — restricted control structures, the expression-size bound, loop depth, iterable caps — are the primary controls and this is a backstop. A buffered channel is required so the abandoned goroutine's send never blocks forever and leaks.

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

- [ ] **Step 1: Write the failing test**

Cover: version params apply when the request omits them; request params win; unrecognised keys land in `ExtraParams`; and — the inherited bug this port **fixes** — `knownSyntheticChatParamKeys` (`reasoning_effort`, `reasoning_max_tokens`) are not misfiled into `ExtraParams` on **either** path. The original guards the Chat path only, so the Responses path files them wrongly.

- [ ] **Step 2–5:** as usual. Commit — `feat(plugin): model parameter merge with the synthetic-key guard on both paths`

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
- **Integration routes drop `variables` at parse.** `integrations/router.go` uses a plain `sonic.Unmarshal` into the integration's own request type, so an unknown top-level field is gone before any plugin runs. On `/openai/*` and friends the **header is the only working transport**. Test that the header path works and document the gap.
- **Large bodies skip the body copy.** `fasthttpToHTTPRequest` (`handlers/middlewares.go`) does not populate `HTTPRequest.Body` above the large-payload threshold. Read `variables` from the **parsed request** in `PreLLMHook`, not from the raw body, and treat the transport hook's body read as an optimisation that may legitimately find nothing.

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
4. A missing variable in the **stored template** returns a short-circuit 400 whose body names the variables, via `engine.AsMissingVariables`.
5. A missing variable in a **client message** leaves that message verbatim and does not fail the request.
6. Variable precedence is version defaults → header → body.
7. The cache key carries tenant, resolved version, partial fingerprint and per-message source hash.
8. One `engine.Budget` is shared across every message of a request.
9. A render exceeding the deadline fails the request rather than hanging.
10. Fallbacks increment a counter and log at debug with the error **truncated** to a bounded length.

- [ ] **Step 2: Run to verify it fails.**

- [ ] **Step 3: Implement.**

Order inside `PreLLMHook`: resolve → build partial set → merge variables → apply params → render stored messages (authored) → prepend → render client messages (untrusted) → return.

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

**Keep the fork diff small.** All logic lives in the module; this task is imports, a case, a config block and an adapter.

Commit — `feat(transports): wire the prompt-templates plugin`

---

## Out of scope

- `/v1/prompts/{id}/render`, the `prompt_partials` table and partial CRUD — Plan 4.
- UI and the `.so` build — Plan 5.
- `extends` via literal-graph cycle detection — still deferred.

## Self-Review

**Spec coverage.** Store interfaces → Task 1. Header/body variables → Tasks 2, 6. Rendering scope → Task 3. Deadline → Task 4. Param merge and the synthetic-key fix → Task 5. `ExtraParams` strip, integration-route gap, large-body caveat → Task 6. Precedence, strict-vs-lenient, cache keying, shared budget, fallback metering, `prompts` coexistence → Task 7. Fork wiring → Task 8.

**Type consistency.** `store.PromptVersion` is defined in Task 1 and consumed in Tasks 5, 7, 8. `TextRenderer` is defined in Task 3 and consumed in Task 7. `Config` is defined in Task 7 and consumed in Task 8.

**Verified, not assumed.** Every Bifrost type in the table above was read from the fork before writing. Two shapes are easy to get wrong and are called out where used: `ChatMessageContent` is a two-field struct with exactly one field populated rather than an interface, and `ChatContentBlock` carries the image URL under `ImageURLStruct.URL` rather than a plain string field.

**Known risk carried forward.** Task 4's deadline abandons rather than stops. Every plan so far has restated this; it stays true, and it is why the engine's structural guards do the real work.
