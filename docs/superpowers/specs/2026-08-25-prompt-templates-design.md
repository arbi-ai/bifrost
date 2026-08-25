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

**"Authored" is a statement about intent, not about privilege.** In a multi-tenant gateway,
prompt authors are customers, not operators, and a customer-authored template running on a
shared process is untrusted input wearing a different label. An earlier revision gave this
environment unrestricted Jinja2 on the assumption that authorship implies trust. That
assumption is wrong for the fork's deployment model, and it left a prompt author able to kill
the shared process with the recursive macro proven fatal above, OOM it with `center(2e8)` or a
long attribute chain, and read other tenants' partials.

The authored environment therefore gets full Jinja2 **expressiveness** but the same
**resource** guards as the untrusted one. The distinction between the two environments is
capability, not safety:

| | Untrusted | Authored |
|---|---|---|
| `include` / `import` / `from`, `macro`, `call`, `do`, `autoescape`, `trans`, `block` | ✗ | ✓ |
| `extends`, `with`, `set`, `filter` | ✗ | ✗ — cut, see below |
| Full filter and method sets | ✗ | ✓ |
| Nested loops | ✗ (depth 1) | ✓ (depth 4, see caveat) |
| Expression size and bracket-depth guards | ✓ | ✓ |
| Iterable and string length caps | ✓ | ✓ |
| Output budget | ✓ | ✓ |
| Macro-recursion depth counter | n/a (no macros) | ✓ |

Loader contains the resolved partial set plus the template source under a per-render random
key. `StrictUndefined = true`.

**The authored environment is not full Jinja2.** Four structures are cut, each for a reason
that cannot be engineered around. Document them as deliberate omissions; authors will notice.

| Cut | Reason |
|---|---|
| `extends` | Recursion happens at **parse** time (`extendsParser` → `p.Extend` → recursive parse), and its `Execute` is a no-op with a non-standard signature, so no runtime wrapper can intercept it. `A extends B, B extends A` is a fatal stack overflow. Recoverable only by building literal-graph cycle detection over the partial set — viable, because `extends` filenames are string literals (`args.Match(tokens.String)`), but not free. |
| `with`, `set`, `filter` | Their node types expose **no** body via exported fields (`wrapper`, `body`, `bodyWrapper` are all unexported), so a loop nest inside them cannot be counted. This is not fixable by adding a walker case. And because `Execute` takes no `context.Context`, the render deadline *abandons* a runaway rather than stopping it — so an uncounted nest is not bounded by anything at all. |

**Two guards are proven implementable and must be built rather than assumed.** Both were
prototyped against gonja v2.9.0 and caught their target without killing the process:

- **Macro recursion.** Wrap the builtin parser, embed `*MacroControlStructure`, override only
  `Execute`, and decorate the `exec.Macro` with a depth counter before it reaches the context.
  Catches direct recursion, mutual recursion (`a`→`b`→`a`), and the emitting variant, while
  three-level legitimate nesting still renders. **The counter must be allocated inside
  `Execute`** — per render — never on the wrapper struct, which is per-parse and shared across
  renders by the cache.
- **Include/import depth.** Same wrapper trick, with the counter in a pointer box stored in the
  Context. A pointer is required: `include` passes the same `*Context` into the nested template
  but `Context.Inherit()` shadows writes, so only mutation through a shared pointer crosses the
  boundary. Catches self-inclusion, mutual inclusion, and **runtime-expression filenames** —
  the case a static pre-pass cannot see.

**`AuthoredGuardLoops` needs two extra roots.** `BlockControlStructure` carries no body on the
node, and macro bodies are likewise not reachable from the statement tree — but
`nodes.Template.Blocks` (`map[string]*Wrapper`) and `nodes.Template.Macros` are both exported.
Walking only `root.Nodes` silently skips every block and macro body.

**The authored loop bound is not a safety property.** Depth 4 × `max_iterable_len` 1 000 is
10¹², and 10¹⁶ once strings are counted. That is an OOM with extra steps, not a limit. The only
honest controls for the authored environment are a runtime step counter or a wall-clock
deadline; the depth×length product is arithmetic and must not be presented as a guarantee.
Plan 2 owns this.

**Authored templates get a larger `max_expression_bytes`** (1–2 KiB rather than 256). A
realistic system-prompt line built from `~` concatenations already measures 207 bytes — 81% of
the untrusted budget. For untrusted text an over-rejection is a harmless verbatim fallback; for
an authored template it is a hard 400 on something that used to work. Validate against real
Portkey templates before shipping.

**`GuardLoops` must not be reused as-is for the authored environment.** It knows only
`For`/`If`/`Raw`/`ControlStructureBlock` and fails closed, so all eleven other control
structures — `macro`, `include`, `extends`, `import`, `with`, `set`, `block`, `call`, `filter`,
`do`, `autoescape` — would be rejected, breaking authored rendering entirely. Plan 2 needs a
separate `AuthoredGuardLoops` with a case per structure. `with` genuinely cannot be walked (all
fields unexported), so either vendor it or accept that loop nests inside `{% with %}` are
uncounted in authored templates, and record which.

A macro-recursion depth counter **is** implementable with exported API: `MacroControlStructure`
embeds the exported `*nodes.Macro`, and `exec.MacroNodeToFunc` returns the exported
`exec.Macro` func type, so the parser can be wrapped and the returned macro decorated with a
per-render depth counter before it reaches the context.

**Partials are tenant-scoped.** `prompt_partials` carries a tenant column and the loader is
built per tenant, so `{% include %}` cannot reach another tenant's content. Neither the
existing `prompts` nor `prompt_versions` tables carry a tenant column today; if the fork's
deployment is genuinely single-admin, record that as an explicit trust assumption rather than
leaving it implied.

`{% include %}`, `{% extends %}`, and `{% import %}` are replaced with custom implementations
that carry a depth counter in the execution context. gonja's own `include` recurses through
`exec.NewTemplate` unconditionally with no depth limit (`builtins/control_structures/include.go`).
For `include` and `import` the filename is an arbitrary runtime expression, so a static pre-pass
over partial references cannot bound them and the depth guard must live inside the control
structure. `extends` is the exception — its filename is a string literal only, so it *is*
statically analysable, which matters because it is the one structure a runtime wrapper cannot
reach.

### Untrusted environment — client-sent messages

Built with `exec.NewControlStructureSet` containing only: `if`, `for`, and `raw`. Everything
else is a parse error, which triggers the verbatim fallback before any execution occurs.

`with` is excluded despite being harmless in itself: `WithControlStructure`'s fields are all
unexported (`location`, `pairs`, `wrapper`), so the loop-depth guard cannot descend into its
body and a loop nest hidden inside `{% with %}` would go uncounted. It buys only
`{% with y = 1 %}`, which end-user text does not need.

**Parse-time rejection is necessary but not sufficient.** Two attack classes never reach the
control-structure layer at all, and both are covered below: the parser itself is
attacker-reachable (see "Pre-parse guards"), and the expression layer can exhaust memory
inside a single `{{ … }}` with no control structure present (see "Filter allowlist").

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

### Pre-parse guards

**The parser is attacker-reachable and is not recursion-safe.** `{{ ((((…1…)))) }}` at 60 000
nesting levels is 120 007 bytes — comfortably under `max_template_bytes` — and causes a fatal
Go stack overflow inside `parser.(*Parser).parseOr`. `[[[[…]]]]` behaves identically. This is
the same unrecoverable class as the recursive macro: `recover()` does not catch it, the process
exits, and it takes every in-flight request with it.

Nothing in the control-structure design touches this. It uses no control structures, so the
restricted set never sees it, and it happens inside `exec.NewTemplate`, so the AST guard — which
runs on the parse tree — never sees it either. The verbatim fallback cannot catch it because
there is no error to return.

Therefore, **before** `exec.NewTemplate` is called on untrusted input, a scan over the raw
source rejects:

| Guard | Default | Rationale |
|---|---|---|
| `max_expression_depth` | 64 | Maximum unmatched `(`, `[`, `{` nesting depth |
| `max_template_bytes` | 64 KiB | Lowered from 256 KiB; defence in depth, not the primary control |

The depth scan is a single pass over the bytes with three counters — cheap enough for the path
that runs on every request. It must not be replaced by a size limit alone: `(` and `[` are both
two bytes per nesting level, and no audit has been done for a denser one-byte-per-level shape,
so a byte cap is a guess about template shape rather than a bound on parser recursion.

### Filter allowlist

The untrusted environment carries a **restricted filter set**, built by the same subtract-from-
builtins mechanism as the control structures. Without it, a 27-byte template with no control
structure allocates a gigabyte:

```
{{ s | center(200000000) }}   →  1091 MB allocated
{{ s * 100000000 }}           →   286 MB allocated
```

The value is materialised in memory and only then handed to the budgeted writer, so the output
cap, the loop guard, and the iterable cap are all bypassed simultaneously.

Excluded from the untrusted filter set: every filter taking a size or count argument that
allocates proportionally to it — `center`, `indent`, `wordwrap`, `truncate`, `format`,
`filesizeformat`, `batch`, `slice`, `list`.

Two things the filter allowlist does **not** cover, both handled structurally on the parse tree
instead (see "Structural expression guards"):

- **String methods.** `{{ s.zfill(200000000) }}` reaches the same sizing operation through
  different syntax and allocates 572 MB. The method sets are unexported and cannot be
  subtracted, so the untrusted environment gets no methods at all.
- **The `*` operator.** gonja exposes exactly five pluggable sets — `Filters`,
  `ControlStructures`, `Tests`, `Context`, `Methods`. Operators are a hardcoded `switch` in the
  unexported `(*Evaluator).evalBinaryExpression`, so **there is no way to intercept or clamp
  them.** An earlier revision of this spec claimed `*` was clamped; that was not implementable
  and the claim was wrong.

### Structural expression guards

A pre-parse byte scan bounds only what is visible in bytes, and cost is not. Two verified
attacks are cheap in bytes and enormous in cost, so both are rejected on the parse tree:

- **`*` repetition** — `{{ s * n }}` is 11 bytes and allocates 9.5 GB. Capping numeric
  *literals* is insufficient: the multiplier can come from `variables`, which `CheckIterables`
  never inspects because it bounds only strings, slices and maps. The operator is therefore
  rejected wholesale for untrusted text.
- **Filter-chain arity** — each `|tojson` roughly doubles its input, so a chain is exponential
  in its length however few bytes it occupies: 24 filters is 175 bytes and yields 33 MB;
  34 filters fits inside `max_expression_bytes` and yields ~34 GB. `max_filter_chain` is 4;
  realistic prompt expressions use at most three.

`escape` survives chaining only because it returns early when its input is already marked safe
— idempotence in that one filter, not a property of the allowlist. Adding a filter now requires
checking that its **output** is bounded by its input, not merely its cost.

### The panic barrier

`Render` wraps execution in `defer recover()`. gonja panics rather than erroring on some
hostile input: a 37-byte `{{ "abcdefghij" * 9999999999999999 }}` reaches `strings.Repeat`,
which panics with `makeslice: len out of range`, and an unrecovered panic in any goroutine
takes down the process.

Unlike the parser's stack overflow this is recoverable, and recovering it converts an
open-ended class — "some gonja path panics on input nobody foresaw" — into an ordinary verbatim
fallback. No enumeration of specific guards can close that class. The barrier stays permanently,
independent of any individual guard.

The authored environment keeps the full builtin filter set; org-written templates are trusted
input.

## Resource limits

| Limit | Default | Mechanism |
|---|---|---|
| `max_output_bytes` | 1 MiB per **request** | Counting `io.Writer`; see caveat below |
| `max_template_bytes` | 256 KiB | Checked before parse |
| `max_include_depth` | 8 | Counter inside the replacement `include`/`extends`/`import`, authored environment only |
| `max_expression_bytes` | 256 | Size of any single `{{ }}` / `{% %}` region; the class-level bound |
| `max_expression_depth` | 32 | Bracket nesting within one expression; guards parser recursion |
| `max_loop_depth` | 1 untrusted / 4 authored | Post-parse AST walk, **fail-closed type switch**; also rejects `{% for … recursive %}` |
| `max_iterable_len` | 1 000 | Variable map validated before render; **strings count by length** |
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

Two properties the bound depends on, both of which were wrong in an earlier revision:

- **The AST walk must fail closed.** A reflective walk over exported fields silently
  under-counts: `IfControlStructure` holds its branches in `Wrappers []*nodes.Wrapper` and
  `WithControlStructure` holds everything in unexported fields, so a three-deep loop nest
  inside `{% if %}` or `{% with %}` was counted as depth zero and executed, allocating 25 GB.
  The walk is an explicit type switch over the allowed control structures that **returns an
  error on any unrecognised node type**, so an unwalkable node is rejected rather than waved
  through. `with` is dropped from the allowed set for exactly this reason.
- **Strings are iterable.** `{% for c in big %}` over a 200 KB string variable runs 200 000
  times and allocates 345 MB, at loop depth 1 — inside the depth limit, needing no bypass.
  `max_template_bytes` does not bound it, since that governs the template, not the variable
  map. String length therefore counts toward `max_iterable_len`.

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
| Nested `{% for %}` over a 20 000-element client array | 4×10⁸ iterations, zero output | `max_iterable_len` + `max_loop_depth` |
| `{{ ((((…1…)))) }}` at 60 000 depth, 120 KB | **Fatal stack overflow in the parser; process exits**, `recover()` cannot catch it | `max_expression_depth`, checked before parse |
| `{{ s \| center(200000000) }}` — 27 bytes, no control structures | 1091 MB allocated before the writer sees a byte | Filter allowlist |
| `{{ s.zfill(200000000) }}` | 572 MB; same sizing operation reached via a string *method*, bypassing the filter allowlist | No methods in the untrusted environment |
| `{{ a.b.b.b… }}` — 4 KB, no brackets, no loops, no variables | 2792 MB, O(n³); passed the bracket-depth guard, the size cap, the loop guard and the iterable cap, emitting zero output | `max_expression_bytes` |
| `{{ s * n }}` — 11 bytes, multiplier from `variables` | 9.5 GB; no operator hook exists in gonja | `*` rejected on the parse tree |
| `{{ s\|tojson\|tojson… }}` ×34 — 247 bytes, inside `max_expression_bytes` | ~34 GB; each filter doubles its input | `max_filter_chain` = 4 |
| `{{ "abcdefghij" * 9999999999999999 }}` — 37 bytes | `panic: makeslice: len out of range`; **process exits** | `defer recover()` in `Render` |
| `{{ s * 100000000 }}` | 286 MB allocated | `*` operator clamped |
| 3-deep loop nest inside `{% if %}` or `{% with %}` | 25 GB; the reflective walker counted it as depth 0 | Fail-closed type-switch walk; `with` dropped |
| `{% for c in big %}` over a 200 KB string variable | 345 MB at loop depth 1 | String length counts toward `max_iterable_len` |

Variable *values* are not re-parsed as templates, so there is no second-order injection through
`variables` — a value containing `{% include %}` renders literally. That property is relied
upon and must be covered by a regression test.

**The lesson this table encodes.** Three review rounds each found that a guard bounded the
specific *shape* last discovered, and the next attack simply used a different shape: blocking
filesystem access did not stop control structures; restricting control structures did not stop
the parser or the expression layer; bounding bracket nesting did not stop attribute chains.
`max_expression_bytes` is the first guard here that bounds a *class* — the cost of any
expression, whatever its syntax — rather than an instance. Prefer that shape of guard. When
adding one, ask what it fails to bound, not what it blocks.

Errors from a failed render are truncated before logging. The attribute-chain attack produces
a 16 MB error string, and the spec requires every fallback to be logged; without truncation the
DoS simply relocates to the log pipeline.

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

The key is **tenant ID plus resolved version number plus a content fingerprint of the partial
set**. Three corrections over the obvious key:

- **Omitting the tenant ID is a cross-tenant data leak, and it fails open.** `exec.NewTemplate`
  stores the loader on the Template (`exec/template.go:43`) and `Execute` renders with that
  compile-time `t.loader` (`:71`) — so a per-tenant loader built at request time is **silently
  ignored on a cache hit**. Verified: tenant B receives tenant A's partial content, with no
  error raised. The tenant column on `prompt_partials` and the per-tenant loader are both
  nullified by the cache unless the key carries the tenant. Fingerprinting partial *names*
  rather than content collides even faster.

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
- `if` (with `else`/`elif`), `for` (with `else` and the `for … if` form), `loop.index`, `raw`,
  nested attribute access, and the permitted filters still parse and render, so the restriction
  costs no stated expressiveness.
- A three-deep loop nest is rejected whether it is bare or hidden inside `{% if %}`, `{% else %}`,
  or `{% elif %}`; an unrecognised node type is rejected rather than walked past.
- Deeply nested `(`/`[` expressions are rejected before parse; the process survives.
- Sizing filters and `*` with a large operand are rejected or clamped.
- Iterating a long string trips `max_iterable_len`.
- A client message cannot read a version default or a partial.
- A variable *value* containing template syntax renders literally (no second-order injection).
- Nested loops over a large client-supplied array trip `max_iterable_len` or `max_loop_depth`.

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
| Goroutine leak from a runaway render | Pre-parse depth scan, restricted control-structure and filter sets, and the fail-closed AST walk are the real controls; the output cap and timeout are backstops that several verified attacks bypass. Monitored, not relied upon |
| A future gonja upgrade reintroducing a bypass | The attack table is a regression suite, not prose; every row is a test. Pin the gonja version and re-run it on every bump |

## Milestones

1. Both render environments and the restricted control-structure set, with the golden-test
   suite and the security regression suite. The environment split is milestone one because
   every later milestone depends on which environment its code runs in.
2. `store/` interfaces, variable precedence, and message rendering.
3. `Plugin` type, hooks, and in-tree `Init`; wire into the fork's `plugins.go`.
4. `/render` endpoint and the `prompt_partials` table plus migration.
5. Partial CRUD API and the UI additions.
6. `cmd/plugin` `.so` build, CI, and distribution docs covering the degraded mode.
