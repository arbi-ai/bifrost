# Prompt Templates Plugin — Design

**Date:** 2026-08-25
**Status:** Approved for planning
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

## Sandbox and resource limits

Every render constructs its template through `exec.NewTemplate(self, cfg, memLoader, env)`,
where `memLoader` is a `loaders.NewMemoryLoader` holding the message source under `/__msg__`
together with the resolved partial set, and nothing else. `gonja.FromString` is banned; a lint
rule and a unit test enforce its absence. The filesystem is not merely restricted but absent —
the verification table above shows `/etc/hosts` returning `unknown path`.

Limits, all configurable with the defaults shown:

| Limit | Default | Mechanism |
|---|---|---|
| `max_output_bytes` | 1 MiB | Counting `io.Writer` passed to `Execute`; aborts synchronously |
| `max_template_bytes` | 256 KiB | Checked before parse |
| `max_include_depth` | 8 | Tracked during partial resolution |
| `max_range_size` | 10 000 | Custom `range` global replacing the builtin |
| `render_timeout` | 250 ms | Render runs in a goroutine; caller abandons on timeout |

**Known limitation, stated deliberately:** `Execute` accepts no `context.Context`, so a
deadline cannot interrupt a render already in flight. The output cap terminates anything that
emits, and the capped `range` global removes the only unbounded non-emitting loop source. The
timeout is a backstop that abandons the goroutine rather than killing it; a render that
escapes both prior controls leaks one goroutine until it completes. Accepted for v1 and
recorded here so it is not rediscovered as a surprise.

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

## Variable sources and precedence

Shallow merge by top-level key, lowest precedence first:

1. Version-declared defaults — `TablePromptVersion.Variables` values
2. `x-bf-prompt-variables` header — JSON object
3. Body `variables` field — JSON object

The body wins because it is the most specific and the most expressive (nested objects and
arrays for `{% for %}`; the header is size-constrained and awkward for documents).

This requires widening `TablePromptVersion.Variables` from keys-only to carry real default
values. The change is backward compatible: every existing value is `""`.

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

**Standard inference routes** — `POST /v1/chat/completions`, `/v1/responses`, and the
integration equivalents accept a top-level `variables` object alongside `messages`, and the
`x-bf-prompt-variables` header. Prompt selection continues to use the existing
`x-bf-prompt-id` / `x-bf-prompt-version` headers.

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

**Partial CRUD** — REST endpoints under the existing prompt-repo API surface.

No dedicated `/v1/prompts/{id}/completions` endpoint. The body-field surface on the standard
routes covers that capability, and adding a second completion path would duplicate streaming,
auth, and governance handling for no gain.

## Model parameters

Unchanged from the `prompts` plugin: the version's `ModelParams` act as defaults and any
param present in the request wins. The existing merge logic — including the `ExtraParams`
reconciliation for keys that are not recognised standard fields — is ported as-is rather than
rewritten, since it already handles the synthetic-key edge cases.

## Coexistence with the `prompts` plugin

Both plugins occupy the same ordering slot and both write
`BifrostContextKeySelectedPromptID`. Running both is unsupported:

- At init, if `prompts` is also enabled, the new plugin logs a warning naming both.
- At request time, if `BifrostContextKeySelectedPromptID` is already set, the new plugin skips
  injection entirely and lets the older plugin's result stand.

This makes a misconfiguration degrade to today's behaviour rather than double-inject.

## Degraded `.so` mode

A `.so` build receives only `Init(config any) error` — no config store, no route registration,
no UI. What still works: variables from the header and body, rendering of client messages and
of any template supplied inline, and partials declared in the plugin's static config. What
does not: the prompt repository, `/render`, and DB-backed partials.

This must be documented prominently. The two distribution paths have materially different
capability sets and conflating them will generate support noise.

## Testing

Test-driven throughout, per the repo's development workflow.

**Standalone module** — table-driven golden tests over pure functions, requiring no running
Bifrost:

- Variables, loops, conditionals, filters, defaults, `{% raw %}`, nested attribute access.
- Partial resolution, bare-name rewriting, depth capping.
- Filesystem traversal blocked; `gonja.FromString` absent from the codebase.
- Strict mode errors and enumerates missing names.
- Lenient fallback returns the client message byte-identical on both parse and exec errors.
- Each resource limit trips at its boundary.
- Precedence: version defaults < header < body.

**Fork** — integration tests through the HTTP transport for the body field, the header, the
`/render` endpoint, partial CRUD, and coexistence with `prompts` enabled.

## Risks

| Risk | Mitigation |
|---|---|
| `.so` ABI breaks on Go or dependency drift | Pin Go 1.26.6; CI builds the `.so` against the fork's exact module graph |
| Render latency on the hot path | Compiled-template cache keyed by prompt ID + version; versions are immutable, so entries never need invalidating |
| Upstream rebase pain | Fork diff held to six files; all logic lives in the standalone module |
| Goroutine leak from a runaway render | Output cap and capped `range` are the real controls; timeout is a backstop. Monitored, not relied upon |

## Milestones

1. Engine and sandbox in the standalone module, with the full golden-test suite.
2. `store/` interfaces, variable precedence, and message rendering.
3. `Plugin` type, hooks, and in-tree `Init`; wire into the fork's `plugins.go`.
4. `/render` endpoint and the `prompt_partials` table plus migration.
5. Partial CRUD API and the UI additions.
6. `cmd/plugin` `.so` build, CI, and distribution docs covering the degraded mode.
