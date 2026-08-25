# Prompt Templates Plugin — Design

**Date:** 2026-08-25
**Status:** Approved 2026-08-25, after adversarial review and revision
**Revision:** The single-environment sandbox was replaced with two isolated render
environments after review found three process-level DoS vectors and a data-exfiltration
primitive reachable from an ordinary chat message. See "Two render environments" and
"Attacks this design must stop".
**Repos:** `arbi-ai/bifrost-prompt-templates` (new module), `arbi-ai/bifrost` (fork)

## Summary

A new Bifrost plugin providing server-side Jinja2 prompt templating at feature parity with
Portkey's Prompt Engineering Studio. It supersedes the existing in-tree `prompts` plugin,
which retrieves and injects stored prompt templates but performs no variable substitution.

The defining requirement: variables supplied by the client render into **both** the stored
template **and** the additional messages the client sends in the same request. Portkey renders
only the stored template, so this is a deliberate superset.

## Goals

- Render stored prompt templates with client-supplied variables at the gateway.
- Render the client's own messages with the same variable set.
- Full Jinja2 expressiveness: conditionals, loops, filters, defaults, partials.
- Never let a template reach the filesystem or exhaust the process.
- Distribute as a standalone Go module usable both in-tree and as a `.so`.

## Non-goals

- Rendering tool/function JSON schemas (v1 renders messages only).
- A prompt-authoring IDE beyond what the existing prompt-repo UI provides.
- Mustache syntax compatibility. Portkey templates using `{{#section}}` / `{{^section}}`
  require a one-time hand translation to `{% if %}` / `{% if not %}`.
- Replacing the `prompts` plugin's code. It stays in the tree, untouched, for back-compat.

## Background: what exists today

Bifrost already ships most of a prompt repository:

- `plugins/prompts/` — resolves a prompt by `x-bf-prompt-id` / `x-bf-prompt-version`, merges
  the version's model params (request wins), and **prepends the template messages verbatim**
  to the chat or Responses input.
- `framework/configstore/tables/` — `prompts`, `prompt_versions`, `prompt_version_messages`,
  `prompt_sessions`. Versions are immutable.
- `transports/bifrost-http/handlers/prompts.go` — CRUD for the repo.
- `ui/app/workspace/prompt-repo/` — folders, versions, playground, and a variables panel.

`TablePromptVersion.Variables` and `TablePromptSession.Variables` are typed
`PromptVariables map[string]string` and commented "Jinja2 variables".

**The gap:** no rendering engine exists anywhere in Go. Substitution happens only in the UI
playground, in `ui/lib/message/variables.ts` — a regex over `{{ var }}` that handles string
values only, skips assistant and tool messages, and supports no loops, conditionals, defaults,
or partials. An API client calling `/v1/chat/completions` with `x-bf-prompt-id` today has
`{{ customer_name }}` forwarded literally to the provider.

## Decisions

| # | Decision | Choice |
|---|---|---|
| 1 | Placement | New plugin superseding `prompts`; `prompts` retained for back-compat |
| 2 | Engine | Jinja2 via `github.com/nikolalohinski/gonja/v2` |
| 3 | Variable transport | Body `variables` field, `x-bf-prompt-variables` header, `/render` endpoint |
| 4 | Render scope | Everything — stored template and client messages — sandboxed and capped |
| 5 | Repo split | Engine library (standalone module) + thin wiring in the fork |
| 6 | Undefined variables | Strict on stored template, verbatim fallback on client messages |

Decision 5 was re-examined against the possibility of avoiding the fork. Confirmed constraints:
`TransportInterceptorMiddleware` is appended only to `inferenceMiddlewares` and passed to
`RegisterInferenceRoutes`; `RegisterAPIRoutes` uses a separate chain that never calls plugin
hooks. The router 404s unknown paths before plugins run, method/path rewrites from a hook are
rejected with 409, and `soloader.go` exposes no route-registration symbol. A plugin therefore
cannot serve a UI or add routes. The fork is accepted deliberately to obtain the
`/v1/prompts/{id}/render` URL, partial CRUD, and a partials UI.

## Verified engine behaviour

All confirmed empirically against gonja v2.9.0 before this spec was written:

| Behaviour | Result |
|---|---|
| `{{ v }}`, `{% if %}`, `{% for %}`, `{% raw %}`, nested attrs, `tojson` | Work |
| `{% include '/name' %}` against an in-memory loader | Works |
| `{% include '/etc/hosts' %}` against an in-memory loader | Blocked — `unknown path` |
| `config.Config.StrictUndefined = true`, missing variable | Errors, including nested `user.city` |
| `{{ nope \| default('x') }}` under strict mode | Renders — the author's opt-out |
| `StrictUndefined = false`, missing variable | Renders `""`, **not** the literal `{{ nope }}` |
| `loaders.NewMemoryLoader` keys | Must begin with `/` |
| `Template.Execute(io.Writer, *Context)` | Exists — enables a synchronous output cap |
| `range` | A plain entry in `builtins.GlobalFunctions`; `Environment.Context` is swappable |

Two consequences drive the design below. First, `gonja.FromString` attaches a filesystem
loader rooted at the working directory, so it must never be used. Second, gonja has no
"leave the literal token in place" mode, so the lenient half of decision 6 needs its own
mechanism.

## Architecture

### `arbi-ai/bifrost-prompt-templates`

Module `github.com/arbi-ai/bifrost-prompt-templates`, Go pinned to **1.26.6** — the `.so` ABI
requires an exact toolchain match with Bifrost. Note that `docs/plugins/writing-go-plugin.mdx`
still instructs plugin authors to pin 1.26.1; `core/go.mod`, `transports/go.mod`, and
`plugins/prompts/go.mod` are all on 1.26.6, so the doc is stale and following it would produce
a `.so` that fails to load.

```
engine/      gonja environment construction, sandbox, caps, render primitives
partials/    partial registry → in-memory loader
render/      message rendering, variable extraction, param merge
store/       PromptStore / PartialStore interfaces — no gorm, no framework import
plugin.go    Plugin: schemas.LLMPlugin + HTTP transport hooks
             Init(ctx, store, cfg, logger) for in-tree use
cmd/plugin/  package main → .so, exporting the free-function hook symbols
```

`store/` is interface-only. That is what lets the same module be backed by the framework
config store in the fork and by nothing at all in a `.so` build.

### `arbi-ai/bifrost` fork

Kept deliberately small so rebasing on upstream `dev` stays cheap:

| File | Change |
|---|---|
| `transports/bifrost-http/server/plugins.go` | import + `case prompttemplates.PluginName` |
| `transports/bifrost-http/lib/config.go` | add to the builtin plugin list |
| `transports/bifrost-http/handlers/prompttemplates.go` | `/v1/prompts/{id}/render`, partial CRUD |
| `transports/config.schema.json` | plugin config block |
| `framework/configstore/tables/promptPartials.go` | new table + migration |
| `ui/` | partials editor, render preview |

## Two render environments

**This is the load-bearing security decision.** Authored content and untrusted content are
rendered in separate gonja environments with different capabilities. An earlier revision of
this spec rendered both in one environment and reasoned that blocking filesystem access made
that safe. That reasoning was wrong: the filesystem was never the interesting attack surface,
and an adversarial review found three unauthenticated process-level DoS vectors and a data
exfiltration primitive that involve no filesystem access at all. Evidence is recorded in the
"Attacks this design must stop" section below.

### Authored environment — stored template messages and partials

Full Jinja2. Loader contains the resolved partial set plus the template source under a
per-render random key. `StrictUndefined = true`.

`{% include %}`, `{% extends %}`, and `{% import %}` are replaced with custom implementations
that carry a depth counter in the execution context. gonja's own `include` recurses through
`exec.NewTemplate` unconditionally with no depth limit (`builtins/control_structures/include.go`),
and the filename is an arbitrary runtime expression, so a static pre-pass over partial
references cannot bound it. The depth guard must live inside the control structure.

### Untrusted environment — client-sent messages

Built with `exec.NewControlStructureSet` containing only: `if`, `for`, `raw`, and `with`.
Everything else is a parse error, which is the correct failure mode — it triggers the verbatim
fallback before any execution occurs.

Excluded, and why each one must be:

| Excluded | Reason |
|---|---|
| `macro`, `call` | Recursive macro → Go stack overflow → **fatal, unrecoverable by `recover()`**; the process dies |
| `filter` block, `set` block (`{% set x %}…{% endset %}`) | Swap `sub.Output` for a `strings.Builder`/`bytes.Buffer`, bypassing the counting writer entirely |
| `include`, `extends`, `import`, `from`, `block` | Read any partial by name, statically or via a client-controlled variable; also unbounded recursion |

`set` is excluded in **both** forms. Only the block form is dangerous, but both are produced by
a single `setParser` and the field distinguishing them (`cs.body`) is unexported, so the safe
assignment form cannot be admitted on its own without vendoring the parser. End-user text has
no need for `{% set %}`, so exclusion is the cheaper trade.

The untrusted loader contains the message source **and nothing else** — no partials, no
sibling messages. Self-inclusion is impossible because the include family is not in the
control-structure set at all.

Variables in the untrusted environment are an explicit allowlist, never the merged map. Two
reasons: version-declared defaults are org-authored content the caller never supplied and must
not be able to read back, and gonja invokes exported Go methods on context values via
reflection, so any handle placed in the context exposes its entire method set.

`gonja.FromString` is banned in both environments — it attaches a filesystem loader rooted at
the working directory. A lint rule and a unit test enforce its absence.

## Resource limits

| Limit | Default | Mechanism |
|---|---|---|
| `max_output_bytes` | 1 MiB per **request** | Counting `io.Writer`; see caveat below |
| `max_template_bytes` | 256 KiB | Checked before parse |
| `max_include_depth` | 8 | Counter inside the replacement `include`/`extends`/`import`, authored environment only |
| `max_loop_depth` | 2 | Post-parse AST walk over `*ForControlStructure`; also rejects `{% for … recursive %}` |
| `max_iterable_len` | 1 000 | Variable map validated before render |
| `render_timeout` | 250 ms | Render runs in a goroutine; caller abandons on timeout |

`max_output_bytes` is a **per-request** budget shared across all messages, not per-message;
otherwise N messages multiply it.

Loop depth and iterable length together replace the earlier `max_range_size`. Capping the
`range` global was theatre: nested loops multiply (three 10-iteration loops = 1000 iterations,
zero output), and `{% for %}` iterates client-supplied arrays from the body `variables` field,
which a `range` cap does not touch at all. Bounding both factors caps total iterations at
`max_iterable_len ^ max_loop_depth` — 10⁶ at the defaults.

A runtime step counter inside the loop would be tighter, but `forParser` is unexported in
`builtins/control_structures` and cannot be wrapped without vendoring it. The depth-plus-length
pair achieves a sound bound using only exported API. `range` is dropped regardless: the
untrusted environment is built with an empty global context, which removes `range`, `lipsum`,
and `cycler`.

**Known limitation, stated deliberately:** `Execute` accepts no `context.Context`, so a
deadline cannot interrupt a render already in flight, and the timeout abandons the goroutine
rather than killing it. The output cap is *not* a general backstop — it is bypassed by any
construct that redirects `sub.Output` into a buffer. Safety therefore rests on the restricted
control-structure set preventing those constructs from ever parsing, with the iteration budget
and output cap as secondary controls. This is why the environment split, not the caps, is the
primary defence.

## Attacks this design must stop

Each verified against gonja v2.9.0. Every one is reachable from an ordinary chat message with
no filesystem access, and none is stopped by the earlier single-environment design.

| Attack | Effect | Stopped by |
|---|---|---|
| `{% macro f(n) %}{{ f(n+1) }}{% endmacro %}{% set _ = f(0) %}` | Go stack overflow; **process exits**, `recover()` cannot catch it | `macro` excluded from untrusted set |
| `{% filter upper %}…{% endfilter %}` around a large expansion | 3.9 GB allocated under a 1 MiB output cap | `filter` block excluded |
| `{% include '/__msg__' %}` (self), or mutually recursive partials | Goroutine leak allocating GB/s until OOM | include family excluded; depth counter in authored env |
| `{% include client_supplied_var %}` | Reads any partial by name; cross-tenant if the registry is global | include family excluded from untrusted set |
| `{{ hidden_default }}` | Prints an org-authored version default the caller never sent | Untrusted env sees an allowlist, not the merged map |
| `{{ some_handle.Dump() }}` | gonja calls exported Go methods by reflection | Never place handles in the context |
| Nested `{% for %}` over a 20 000-element client array | 4×10⁸ iterations, zero output | `max_total_iterations` |

Variable *values* are not re-parsed as templates, so there is no second-order injection through
`variables` — a value containing `{% include %}` renders literally. That property is relied
upon and must be covered by a regression test.

## Undefined-variable semantics

**Stored template messages** render with `StrictUndefined = true`. A failure returns HTTP 400
with the missing names enumerated:

```json
{ "error": { "type": "missing_prompt_variables", "variables": ["company", "tier"] } }
```

Rationale: a prompt version declares its variables, so a missing one is an authoring or caller
bug. Authors opt a variable out with `{{ x | default('y') }}`, which satisfies strict mode.

**Client-sent messages** also render with `StrictUndefined = true`, but **any parse or
execution error falls back to the original message, byte for byte**. This yields the
"leave the literal token" semantics without walking the AST, and it means an end user typing
`{{lol}}` or `{% if %}` into a chat box degrades to passthrough instead of returning 400 on
production traffic.

The asymmetry is the point: the stored template is authored and declared, so silence is wrong;
client messages are unauthored text, so failing loudly is wrong.

Discarding partial output is deliberate. The renderer writes incrementally, so a message that
errors mid-render has already emitted bytes; returning that truncated text to the provider
would be worse than either rendering or not rendering. The fallback returns the original
message byte for byte.

**Every fallback increments a counter and emits a debug log with the parse or exec error.**
Silence is the design goal for the *caller*, not for the operator: an attacker probing the
restricted control-structure set produces nothing but fallbacks, and without metering that
traffic is invisible. A sustained fallback rate on one virtual key is the signal that someone
is testing the sandbox.

## Variable sources and precedence

Shallow merge by top-level key, lowest precedence first:

1. Version-declared defaults — `TablePromptVersion.Variables` values
2. `x-bf-prompt-variables` header — JSON object
3. Body `variables` field — JSON object

The body wins because it is the most specific and the most expressive (nested objects and
arrays for `{% for %}`; the header is size-constrained and awkward for documents).

`TablePromptVersion.Variables` is widened from `map[string]string` to `map[string]any`. The
`string` typing cannot hold the arrays and objects that `{% for %}` and nested access require,
so version-declared defaults could otherwise never carry list values.

**A declared variable whose default is empty is treated as absent, not as a value.** Today
every row's value is `""` (the column stores declared *names*). A naive merge would place
every declared name in the map with value `""`, so `StrictUndefined` would never fire and the
400-with-missing-names behaviour — the entire justification for the strict/lenient asymmetry —
would silently degrade into rendering empty strings for exactly the case it exists to catch.
The merge must therefore skip empty defaults rather than treat presence as satisfaction. This
is a required regression test, not an implementation detail.

## Rendering scope

Rendered:

- Text content of every message, all roles, stored template and client alike.
- Text parts of multimodal content.
- Image **URL** strings — templated signed URLs are a legitimate use case.

Not rendered, passed through untouched:

- Base64 and binary content parts.
- Tool and function JSON schemas (out of scope for v1).

## Partials

New table `prompt_partials`: `id`, `name`, `content`, `version`, timestamps. Partials are
loaded into the in-memory loader keyed as `/name`. Bare references in author-written templates
(`{% include 'brand_voice' %}`) are rewritten to `/brand_voice` during resolution so authors
never write the leading slash. Resolution is depth-capped per the limits table.

## HTTP surfaces

**Native inference routes** — `POST /v1/chat/completions` and `/v1/responses` accept a
top-level `variables` object alongside `messages`, plus the `x-bf-prompt-variables` header.
Prompt selection continues to use the existing `x-bf-prompt-id` / `x-bf-prompt-version`
headers.

Three transport details that are easy to get wrong and each need a test:

- **`variables` must be stripped from `ExtraParams`.** `extractExtraParams`
  (`handlers/inference.go`) sweeps every field absent from `chatParamsKnownFields` /
  `responsesParamsKnownFields` into `Params.ExtraParams`, and `variables` is in neither list.
  Left in place, a caller sending `x-bf-passthrough-extra-params: true` gets `variables`
  merged verbatim into the outgoing provider body and a 400 from upstream. The plugin deletes
  the key after reading it.
- **Integration routes (`/openai/*`, etc.) discard `variables` at parse.** `integrations/router.go`
  uses a plain `sonic.Unmarshal` into the integration's own request type, so an unknown
  top-level field is dropped before any plugin runs. On those routes the header is the only
  working transport. This must be documented rather than silently under-delivered.
- **Large bodies skip the body copy.** `fasthttpToHTTPRequest` (`handlers/middlewares.go`)
  does not populate `HTTPRequest.Body` above the large-payload threshold, so a plugin reading
  the raw body in `HTTPTransportPreHook` sees nothing and the variables vanish with no error.
  Read `variables` from the parsed request in `PreLLMHook` rather than from the raw body, and
  bound the header path by `ServerConfig.ReadBufferSize`.

**`POST /v1/prompts/{id}/render`** — returns the fully rendered messages and merged params
without calling a provider, in Portkey's response shape:

```json
{
  "success": true,
  "data": {
    "model": "openai/gpt-5",
    "temperature": 0.2,
    "messages": [{ "role": "system", "content": "You have priority support." }]
  }
}
```

`/render` is registered on the **inference** chain, so virtual-key API clients authenticate
exactly as they do for completions. This is deliberate: the existing prompt CRUD lives at
`/api/prompt-repo/*` on the API chain behind dashboard/session auth, and registering `/render`
there would 401 every API client and break the Portkey-parity use case it exists to serve.
The two surfaces have different callers and therefore different auth chains.

**Partial CRUD** — REST endpoints at `/api/prompt-repo/partials`, on the API chain alongside
the existing prompt CRUD, since these are authoring operations.

No dedicated `/v1/prompts/{id}/completions` endpoint. The body-field surface on the standard
routes covers that capability, and adding a second completion path would duplicate streaming,
auth, and governance handling for no gain.

## Model parameters

Unchanged from the `prompts` plugin: the version's `ModelParams` act as defaults and any
param present in the request wins. The existing merge logic — including the `ExtraParams`
reconciliation for keys that are not recognised standard fields — is ported, since it already
handles the synthetic-key edge cases and the marshal-merge-unmarshal round trip is safe (all
`ChatParameters` fields are `omitempty` pointers, so request zero-values cannot clobber
version defaults).

One inherited bug is fixed rather than ported: `applyVersionParamsToChatRequest` guards
`knownSyntheticChatParamKeys`, but `applyVersionParamsToResponsesRequest` has no equivalent
guard, so synthetic keys land in `ExtraParams` on the Responses path only. The port applies
the guard to both.

## Coexistence with the `prompts` plugin

Both plugins write `BifrostContextKeySelectedPromptID`. **Enabling both is a startup error.**

A context-key guard alone does not work. It only holds if `prompts` runs first, and both
plugins would sit at `builtin` order 2, where `SortAndRebuildPlugins` breaks ties by
registration order (`lib/config.go`). Register the new plugin first and the guard inverts: it
sets the key, then `prompts` runs — and `prompts` has no reciprocal guard, since
`PreLLMHook` (`plugins/prompts/main.go`) never reads that key — so both templates inject and
the user gets a silently doubled prompt.

Since `prompts` cannot be changed without growing the fork diff, the new plugin refuses to
initialise when `prompts` is also enabled, naming both in the error. A hard failure at boot is
correct here: the alternative is a corrupted prompt on every request, discovered in
production. Note that `prompts` is disabled under enterprise and requires a config store
(`server/plugins.go`), so the conflict does not arise in every deployment.

## Degraded `.so` mode

A `.so` build receives only `Init(config any) error` — no config store, no route registration,
no UI. What still works: variables from the header and body, rendering of client messages and
of any template supplied inline, and partials declared in the plugin's static config. What
does not: the prompt repository, `/render`, and DB-backed partials.

This must be documented prominently. The two distribution paths have materially different
capability sets and conflating them will generate support noise.

## Template cache

Stored templates are cached compiled. A shared `*exec.Template` is safe for concurrent
`Execute` — it builds a fresh `Environment` with `Context.Inherit()` per call, and `{% set %}`
does not leak across renders — so no per-request compilation or locking is needed.

The key is **resolved version number plus a fingerprint of the partial set**, not the
requested version plus prompt ID. Two corrections over the obvious key:

- **"Latest" is not a version.** With no `x-bf-prompt-version` header the resolver returns
  `prompt.LatestVersion`. Keying on the requested version — absent, i.e. 0 — would serve the
  stale compiled template forever after a new version is published.
- **Partials are baked into the parse tree and are mutable.** `{% extends %}` and `{% import %}`
  resolve at parse time, so a cached template pins whatever partial content it was compiled
  against. Editing a partial through the new CRUD API would otherwise never invalidate it.
  The fingerprint makes partial edits invalidate the entries that depend on them.

Version immutability makes the first component safe once resolved; it says nothing about the
second.

## Testing

Test-driven throughout, per the repo's development workflow.

**Standalone module** — table-driven golden tests over pure functions, requiring no running
Bifrost:

- Variables, loops, conditionals, filters, defaults, `{% raw %}`, nested attribute access.
- Partial resolution, bare-name rewriting, depth capping.
- Filesystem traversal blocked; `gonja.FromString` absent from the codebase.
- Strict mode errors and enumerates missing names.
- Lenient fallback returns the client message byte-identical on both parse and exec errors,
  including when the render had already emitted output before failing.
- Each resource limit trips at its boundary; `max_output_bytes` is per-request, not per-message.
- Precedence: version defaults < header < body, **and an empty version default counts as
  absent so strict mode still fires**.

Security regression suite — one test per row of "Attacks this design must stop", each
asserting a *parse* error in the untrusted environment rather than a caught runtime failure:

- Recursive `macro`, `call`, `filter` block, `set` block, and every member of the include
  family are rejected at parse.
- `if` / `for` / `raw` / `with` / assignment-`set` still parse and render, so the restriction
  costs no stated expressiveness.
- A client message cannot read a version default or a partial.
- A variable *value* containing template syntax renders literally (no second-order injection).
- Nested loops over a large client-supplied array trip `max_total_iterations`.

The macro case cannot be tested in-process, since the failure it guards against is a fatal
stack overflow that `recover()` does not catch. Assert the parse rejection; if a subprocess
test of the unrestricted behaviour is wanted as documentation, it must run via `os/exec` and
assert a non-zero exit.

**Fork** — integration tests through the HTTP transport for the body field, the header, the
`/render` endpoint, partial CRUD, and coexistence with `prompts` enabled.

## Risks

| Risk | Mitigation |
|---|---|
| `.so` ABI breaks on Go or dependency drift | Pin Go 1.26.6; CI builds the `.so` against the fork's exact module graph |
| Render latency on stored templates | Compiled-template cache keyed by **resolved** version number + a fingerprint of the partial set (see below) |
| Render latency on client messages | Unmitigated and unmitigatable by caching — client message templates are arbitrary strings. This is the path running on 100% of requests, so the restricted parser must be cheap; benchmark it |
| Upstream rebase pain | Fork diff held to six files; all logic lives in the standalone module |
| Goroutine leak from a runaway render | Output cap and capped `range` are the real controls; timeout is a backstop. Monitored, not relied upon |

## Milestones

1. Both render environments and the restricted control-structure set, with the golden-test
   suite and the security regression suite. The environment split is milestone one because
   every later milestone depends on which environment its code runs in.
2. `store/` interfaces, variable precedence, and message rendering.
3. `Plugin` type, hooks, and in-tree `Init`; wire into the fork's `plugins.go`.
4. `/render` endpoint and the `prompt_partials` table plus migration.
5. Partial CRUD API and the UI additions.
6. `cmd/plugin` `.so` build, CI, and distribution docs covering the degraded mode.
