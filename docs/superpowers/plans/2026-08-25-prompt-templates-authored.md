# Prompt Templates — Authored Environment Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the authored render environment — org-written prompt templates with partials, macros and inheritance — with the same resource guards as the untrusted path and correct tenant isolation.

**Architecture:** The authored environment keeps Jinja2 *expressiveness* the untrusted one cuts (`macro`, `include`, `import`, `block`, `call`, nested loops) but takes the same *resource* guards. Four structures are cut outright because they cannot be guarded with exported API. Two runtime depth counters — macro recursion and include depth — are implemented as control-structure wrappers. Partials and the compiled-template cache are both tenant-scoped.

**Tech Stack:** Go 1.26.6, `github.com/nikolalohinski/gonja/v2` v2.9.0, `stretchr/testify`.

**Spec:** `docs/superpowers/specs/2026-08-25-prompt-templates-design.md`

**Predecessor:** `docs/superpowers/plans/2026-08-25-prompt-templates-engine.md` — complete, 109 tests passing. This plan consumes `Budget`, `GuardExpressions`, `GuardExpressionNodes`, `CheckIterables`, `Limits`, `MissingVariablesError` and `AsMissingVariables` from it.

**Repo:** `arbi-ai/bifrost-prompt-templates` (at `/Users/cameron/projects/neuroscale/oss/bifrost-prompt-templates`).

## Global Constraints

- Go pinned to **1.26.6**. `gonja.FromString` remains banned repo-wide; `engine/banned_test.go` enforces it.
- No import of `github.com/maximhq/bifrost/*` in this module.
- Memory-loader keys **must** begin with `/`.
- **Authored failures are hard errors, not verbatim fallbacks.** That asymmetry is the design: an authored template is declared and version-controlled, so a missing variable or a guard rejection is a bug the author must see. The untrusted path's fallback exists because end-user text is unauthored.
- The panic barrier applies here too. Authored input is customer-authored, not operator-authored.

**Four structures are cut from the authored set and must stay cut:**

| Cut | Reason |
|---|---|
| `extends` | Recursion happens at **parse** time (`extendsParser` → `p.Extend` → recursive parse) and its `Execute` has a non-standard one-parameter signature, so no runtime wrapper can intercept it. Mutual `extends` is a fatal stack overflow. Recoverable only by literal-graph cycle detection, which is out of scope for this plan — `extends` filenames are string literals, so that remains open as a future option. |
| `with`, `set`, `filter` | Their nodes expose **no** body through exported fields (`wrapper`, `body`, `bodyWrapper` are unexported), so a loop nest inside them cannot be counted. Not fixable by adding a walker case. Because `Execute` takes no `context.Context`, the render deadline *abandons* a runaway rather than stopping it, so an uncounted nest is bounded by nothing. |

Document these as deliberate omissions in user-facing docs. Authors will notice.

---

### Task 1: Authored limits and control-structure set

**Files:**
- Create: `engine/authored_controlstructures.go`
- Test: `engine/authored_controlstructures_test.go`

**Interfaces:**
- Consumes: `engine.Limits`, `engine.DefaultLimits`.
- Produces:
  - `var AuthoredAllowed []string`
  - `func AuthoredControlStructures() *exec.ControlStructureSet`
  - `func AuthoredLimits() Limits`

- [ ] **Step 1: Write the failing test**

Create `engine/authored_controlstructures_test.go`:

```go
package engine_test

import (
	"testing"

	"github.com/arbi-ai/bifrost-prompt-templates/engine"
	"github.com/nikolalohinski/gonja/v2/builtins"
	"github.com/nikolalohinski/gonja/v2/config"
	"github.com/nikolalohinski/gonja/v2/exec"
	"github.com/nikolalohinski/gonja/v2/loaders"
	"github.com/stretchr/testify/require"
)

func parseAuthored(t *testing.T, src string) error {
	t.Helper()
	ldr, err := loaders.NewMemoryLoader(map[string]string{"/__tpl__": src, "/p": "partial"})
	require.NoError(t, err)
	env := &exec.Environment{
		Context:           exec.NewContext(map[string]any{}),
		Filters:           builtins.Filters,
		Tests:             builtins.Tests,
		ControlStructures: engine.AuthoredControlStructures(),
		Methods:           builtins.Methods,
	}
	_, err = exec.NewTemplate("/__tpl__", config.New(), ldr, env)
	return err
}

// These four cannot be guarded with exported API. See the plan's Global
// Constraints for why each one is unfixable rather than merely unimplemented.
func TestAuthoredCutsUnguardableStructures(t *testing.T) {
	cases := map[string]string{
		"extends":   `{% extends '/p' %}`,
		"with":      `{% with y = 1 %}{{ y }}{% endwith %}`,
		"set_block": `{% set x %}y{% endset %}`,
		"set":       `{% set x = 1 %}`,
		"filter":    `{% filter upper %}x{% endfilter %}`,
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			require.Error(t, parseAuthored(t, src))
		})
	}
}

// The authored environment's whole purpose is the expressiveness the untrusted
// one cuts. These must all parse.
func TestAuthoredKeepsRichStructures(t *testing.T) {
	cases := map[string]string{
		"if":      `{% if a %}x{% endif %}`,
		"for":     `{% for a in xs %}{{ a }}{% endfor %}`,
		"raw":     `{% raw %}{{ lit }}{% endraw %}`,
		"macro":   `{% macro greet(n) %}Hi {{ n }}{% endmacro %}`,
		"include": `{% include '/p' %}`,
		"import":  `{% import '/p' as p %}`,
		"from":    `{% from '/p' import thing %}`,
		"block":   `{% block b %}x{% endblock %}`,
		"call":    `{% macro m() %}{{ caller() }}{% endmacro %}{% call m() %}x{% endcall %}`,
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			require.NoError(t, parseAuthored(t, src))
		})
	}
}

// Authored templates get a larger expression budget: a realistic system-prompt
// line built from ~ concatenations already measures 207 bytes, 81% of the
// untrusted 256-byte budget. An over-rejection there is a harmless verbatim
// fallback; here it is a hard error on a template that used to work.
func TestAuthoredLimitsAreLooserThanUntrusted(t *testing.T) {
	a, u := engine.AuthoredLimits(), engine.DefaultLimits()
	require.Greater(t, a.MaxExpressionBytes, u.MaxExpressionBytes)
	require.Greater(t, a.MaxLoopDepth, u.MaxLoopDepth)
	require.Positive(t, a.MaxMacroDepth)
	require.Positive(t, a.MaxIncludeDepth)
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./engine/ -run TestAuthored -v`
Expected: FAIL — `undefined: engine.AuthoredControlStructures`.

- [ ] **Step 3: Implement**

Add `MaxMacroDepth` and `MaxIncludeDepth` to `Limits` in `engine/untrusted.go` (they are zero for untrusted, which permits neither construct), then create `engine/authored_controlstructures.go`:

```go
package engine

import (
	"github.com/nikolalohinski/gonja/v2/builtins"
	"github.com/nikolalohinski/gonja/v2/exec"
	"github.com/nikolalohinski/gonja/v2/parser"
)

// AuthoredAllowed is the control-structure set for org-authored templates.
//
// Cut deliberately, each because it cannot be guarded with exported API:
//
//	extends            recursion happens at PARSE time and its Execute has a
//	                   non-standard signature, so no wrapper can intercept it;
//	                   mutual extends is a fatal stack overflow
//	with, set, filter  bodies unreachable via exported fields, so a loop nest
//	                   inside them cannot be counted — and the render deadline
//	                   abandons rather than stops a runaway, so it is unbounded
//
// macro, include, import and from are KEPT because both of their dangerous
// behaviours — unbounded recursion and unbounded inclusion depth — are
// interceptable at runtime. See authored_macro.go and authored_include.go.
var AuthoredAllowed = []string{
	"if", "for", "raw", "macro", "call", "do",
	"autoescape", "trans", "block", "include", "import", "from",
	"break", "continue",
}

// AuthoredControlStructures builds the authored set from gonja's builtins,
// then replaces macro and the include family with depth-counting wrappers.
func AuthoredControlStructures() *exec.ControlStructureSet {
	allowed := make(map[string]parser.ControlStructureParser, len(AuthoredAllowed))
	for _, name := range AuthoredAllowed {
		if p, ok := builtins.ControlStructures.Get(name); ok {
			allowed[name] = p
		}
	}
	// Wrappers are installed by Tasks 3 and 4; until then the builtin parsers
	// are used unchanged and the depth counters are absent.
	installMacroGuard(allowed)
	installIncludeGuard(allowed)
	return exec.NewControlStructureSet(allowed)
}

// AuthoredLimits are the resource bounds for org-authored templates. They are
// looser than DefaultLimits on expressiveness axes and identical on the axes
// that bound cost.
func AuthoredLimits() Limits {
	l := DefaultLimits()
	// A realistic system-prompt line built from ~ concatenations measures 207
	// bytes — 81% of the untrusted budget. An over-rejection is a hard error
	// here, not a fallback.
	l.MaxExpressionBytes = 2048
	// Nested loops are legitimate in authored templates (rendering a table from
	// a list of lists). NOTE: depth 4 x MaxIterableLen is 10^12, which is NOT a
	// safety property — see the spec. The real controls for authored templates
	// are the output budget and the deadline.
	l.MaxLoopDepth = 4
	l.MaxMacroDepth = 16
	l.MaxIncludeDepth = 8
	return l
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./engine/ -run TestAuthored -v`
Expected: PASS. Create `installMacroGuard` and `installIncludeGuard` as no-op stubs in this task so the package compiles; Tasks 3 and 4 fill them in.

- [ ] **Step 5: Commit**

```bash
git add engine/authored_controlstructures.go engine/authored_controlstructures_test.go engine/untrusted.go
git commit -m "feat(engine): authored control-structure set and limits"
```

---

### Task 2: AuthoredGuardLoops

**Files:**
- Create: `engine/authored_astguard.go`
- Test: `engine/authored_astguard_test.go`

**Interfaces:**
- Consumes: Task 1's set.
- Produces: `func AuthoredGuardLoops(root *nodes.Template, maxDepth int) error`

`GuardLoops` cannot be reused: it knows only `For`/`If`/`Raw`/`ControlStructureBlock` and fails closed, so all eleven other authored structures would be rejected — the same total-functionality break the untrusted walker had before its `ControlStructureBlock` case was added.

**Two extra roots are mandatory.** `BlockControlStructure` carries no body on the node (`location`, `name` unexported) and macro bodies are likewise unreachable from the statement tree. Both are reachable from the template root instead: `nodes.Template.Blocks` (`BlockSet`, i.e. `map[string]*Wrapper`) and `nodes.Template.Macros` (`map[string]*Macro`, whose `Wrapper` field is exported). Walking only `root.Nodes` silently skips every block and macro body — a loop nest inside a macro would be uncounted.

- [ ] **Step 1: Write the failing test**

Create `engine/authored_astguard_test.go`:

```go
package engine_test

import (
	"testing"

	"github.com/arbi-ai/bifrost-prompt-templates/engine"
	"github.com/nikolalohinski/gonja/v2/builtins"
	"github.com/nikolalohinski/gonja/v2/config"
	"github.com/nikolalohinski/gonja/v2/exec"
	"github.com/nikolalohinski/gonja/v2/loaders"
	"github.com/stretchr/testify/require"
)

func parseTreeAuthored(t *testing.T, src string) *exec.Template {
	t.Helper()
	ldr, err := loaders.NewMemoryLoader(map[string]string{"/__tpl__": src, "/p": "partial"})
	require.NoError(t, err)
	env := &exec.Environment{
		Context:           exec.NewContext(map[string]any{}),
		Filters:           builtins.Filters,
		Tests:             builtins.Tests,
		ControlStructures: engine.AuthoredControlStructures(),
		Methods:           builtins.Methods,
	}
	tpl, err := exec.NewTemplate("/__tpl__", config.New(), ldr, env)
	require.NoError(t, err)
	return tpl
}

// Regression for the failure mode that broke the untrusted walker twice: a
// fail-closed switch missing a case rejects everything.
func TestAuthoredGuardAcceptsEveryAllowedStructure(t *testing.T) {
	for _, src := range []string{
		`{% if a %}x{% endif %}`,
		`{% for a in xs %}{{ a }}{% endfor %}`,
		`{% raw %}{{ lit }}{% endraw %}`,
		`{% macro greet(n) %}Hi {{ n }}{% endmacro %}`,
		`{% include '/p' %}`,
		`{% import '/p' as p %}`,
		`{% from '/p' import thing %}`,
		`{% block b %}x{% endblock %}`,
		`{% do xs.append(1) %}`,
		`{% autoescape true %}{{ a }}{% endautoescape %}`,
		`plain {{ v }}`,
	} {
		tpl := parseTreeAuthored(t, src)
		require.NoError(t, engine.AuthoredGuardLoops(tpl.Root(), 4), src)
	}
}

func TestAuthoredGuardRejectsDeepNesting(t *testing.T) {
	src := `{% for a in xs %}{% for b in xs %}{% for c in xs %}{% for d in xs %}{% for e in xs %}` +
		`{% endfor %}{% endfor %}{% endfor %}{% endfor %}{% endfor %}`
	tpl := parseTreeAuthored(t, src)
	require.ErrorIs(t, engine.AuthoredGuardLoops(tpl.Root(), 4), engine.ErrLoopTooDeep)
}

// Macro bodies are NOT reachable from root.Nodes. Walking only the statement
// tree would count this nest as depth zero.
func TestAuthoredGuardWalksMacroBodies(t *testing.T) {
	src := `{% macro m() %}` +
		`{% for a in xs %}{% for b in xs %}{% for c in xs %}{% for d in xs %}{% for e in xs %}` +
		`{% endfor %}{% endfor %}{% endfor %}{% endfor %}{% endfor %}` +
		`{% endmacro %}`
	tpl := parseTreeAuthored(t, src)
	require.ErrorIs(t, engine.AuthoredGuardLoops(tpl.Root(), 4), engine.ErrLoopTooDeep)
}

// Block bodies live in root.Blocks, not on the node at all.
func TestAuthoredGuardWalksBlockBodies(t *testing.T) {
	src := `{% block b %}` +
		`{% for a in xs %}{% for b in xs %}{% for c in xs %}{% for d in xs %}{% for e in xs %}` +
		`{% endfor %}{% endfor %}{% endfor %}{% endfor %}{% endfor %}` +
		`{% endblock %}`
	tpl := parseTreeAuthored(t, src)
	require.ErrorIs(t, engine.AuthoredGuardLoops(tpl.Root(), 4), engine.ErrLoopTooDeep)
}

func TestAuthoredGuardRejectsRecursiveLoop(t *testing.T) {
	tpl := parseTreeAuthored(t, `{% for a in xs recursive %}{{ a }}{% endfor %}`)
	require.ErrorIs(t, engine.AuthoredGuardLoops(tpl.Root(), 4), engine.ErrRecursiveLoop)
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./engine/ -run TestAuthoredGuard -v`
Expected: FAIL — `undefined: engine.AuthoredGuardLoops`.

- [ ] **Step 3: Implement**

Create `engine/authored_astguard.go` with an explicit case per structure in `AuthoredAllowed`, plus `default: return ErrUnwalkableNode`. Walk in this order:

```go
func AuthoredGuardLoops(root *nodes.Template, maxDepth int) error {
	if root == nil {
		return nil
	}
	if err := authoredWalkNodes(root.Nodes, 0, maxDepth); err != nil {
		return err
	}
	// Block bodies are not on the node; they live only here.
	for _, wrapper := range root.Blocks {
		if err := authoredWalkWrapper(wrapper, 0, maxDepth); err != nil {
			return err
		}
	}
	// Macro bodies are likewise unreachable from the statement tree.
	for _, macro := range root.Macros {
		if macro == nil {
			continue
		}
		if err := authoredWalkWrapper(macro.Wrapper, 0, maxDepth); err != nil {
			return err
		}
	}
	return nil
}
```

Model `authoredWalkNode` on `walkNode` in `engine/astguard.go` — same `*nodes.ControlStructureBlock` unwrapping, same `For` depth handling (`BodyWrapper` at depth+1, `EmptyWrapper` at depth), same `If` sibling-branch handling. Add cases for `MacroControlStructure` (walk `.Macro.Wrapper`), `CallControlStructure` (`.Body`), `AutoescapeControlStructure` (`.Wrapper`), `TransControlStructure` (`.SingularBody`, `.PluralBody`), and leaf cases for `do`, `block`, `include`, `import`, `from`, `break`, `continue`.

**Implementer note:** confirm each concrete type name and field against the module cache before writing the case — `builtins/control_structures/`. Do not guess; a wrong type name compiles to a missing case, which fails closed and breaks authored rendering silently until a test catches it. That is precisely how the untrusted walker broke twice.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./engine/ -run TestAuthoredGuard -v`
Expected: PASS. `TestAuthoredGuardAcceptsEveryAllowedStructure` is the one that matters — it is the regression for the fail-closed break.

- [ ] **Step 5: Commit**

```bash
git add engine/authored_astguard.go engine/authored_astguard_test.go
git commit -m "feat(engine): authored loop guard with macro and block roots"
```

---

### Task 3: Macro recursion depth counter

`{% macro f(n) %}{{ f(n+1) }}{% endmacro %}{% set _ = f(0) %}` is a **fatal, unrecoverable** stack overflow — `recover()` does not catch it and the process exits. This counter is what makes keeping `macro` defensible.

**Files:**
- Create: `engine/authored_macro.go`
- Test: `engine/authored_macro_test.go`

**Interfaces:**
- Produces: `installMacroGuard(map[string]parser.ControlStructureParser)`, `var ErrMacroTooDeep`

- [ ] **Step 1: Write the failing test**

Create `engine/authored_macro_test.go`:

```go
package engine_test

import (
	"testing"

	"github.com/arbi-ai/bifrost-prompt-templates/engine"
	"github.com/stretchr/testify/require"
)

// Direct and mutual recursion are both fatal without the counter. The test
// asserting the process survives is the point: an unguarded run exits(2).
func TestAuthoredMacroRecursionIsBounded(t *testing.T) {
	cases := map[string]string{
		"direct":   `{% macro f(n) %}{{ f(n) }}{% endmacro %}{{ f(1) }}`,
		"mutual":   `{% macro a(n) %}{{ b(n) }}{% endmacro %}{% macro b(n) %}{{ a(n) }}{% endmacro %}{{ a(1) }}`,
		"emitting": `{% macro f(n) %}x{{ f(n) }}{% endmacro %}{{ f(1) }}`,
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := renderAuthored(t, src, map[string]any{})
			require.Error(t, err)
			require.ErrorIs(t, err, engine.ErrMacroTooDeep)
		})
	}
}

func TestAuthoredLegitimateMacrosStillWork(t *testing.T) {
	out, err := renderAuthored(t, `{% macro greet(n) %}Hi {{ n }}{% endmacro %}{{ greet('Ada') }}`, map[string]any{})
	require.NoError(t, err)
	require.Equal(t, "Hi Ada", out)
}

// Three-level nesting is legitimate and must not trip the counter.
func TestAuthoredNestedMacrosStillWork(t *testing.T) {
	src := `{% macro c(x) %}<{{ x }}>{% endmacro %}` +
		`{% macro b(x) %}{{ c(x) }}{% endmacro %}` +
		`{% macro a(x) %}{{ b(x) }}{% endmacro %}{{ a('hi') }}`
	out, err := renderAuthored(t, src, map[string]any{})
	require.NoError(t, err)
	require.Equal(t, "<hi>", out)
}

// The counter must reset between renders. Allocating it on the wrapper struct
// — which is per-PARSE and shared across renders by the cache — would leak
// depth from one request into the next and eventually reject valid templates.
func TestAuthoredMacroDepthResetsBetweenRenders(t *testing.T) {
	src := `{% macro greet(n) %}Hi {{ n }}{% endmacro %}{{ greet('Ada') }}`
	for i := 0; i < 50; i++ {
		out, err := renderAuthored(t, src, map[string]any{})
		require.NoError(t, err, "render %d", i)
		require.Equal(t, "Hi Ada", out)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./engine/ -run TestAuthoredMacro -v`
Expected: FAIL — `undefined: engine.ErrMacroTooDeep` (and `renderAuthored`, which Task 7 provides; stub it in this task's test file and delete the stub in Task 7).

- [ ] **Step 3: Implement**

Create `engine/authored_macro.go`. The mechanism, all exported API:

```go
// guardedMacro wraps gonja's MacroControlStructure, overriding only Execute.
type guardedMacro struct {
	*controlStructures.MacroControlStructure
	maxDepth int
}

func (g *guardedMacro) Execute(r *exec.Renderer, tag *nodes.ControlStructureBlock) error {
	macro, err := exec.MacroNodeToFunc(g.Macro, r)
	if err != nil {
		return errors.Wrapf(err, "unable to parse macro '%s'", g.Name)
	}

	// CRITICAL: depth is allocated HERE, inside Execute, so it is per-render.
	// Putting it on guardedMacro would share it across every render of a cached
	// template — depth would leak between requests and eventually reject valid
	// input.
	depth := 0
	wrapped := exec.Macro(func(params *exec.VarArgs) *exec.Value {
		if depth >= g.maxDepth {
			return exec.AsValue(ErrMacroTooDeep)
		}
		depth++
		defer func() { depth-- }()
		return macro(params)
	})

	r.Environment.Context.Set(g.Name, wrapped)
	return nil
}
```

`installMacroGuard` replaces the `"macro"` entry with a parser that calls the builtin `macroParser`, type-asserts the result to `*controlStructures.MacroControlStructure`, and returns `&guardedMacro{...}`.

**Why interception works:** a recursive call resolves the macro name through the render context, and the context holds the *wrapped* func — so recursion re-enters the counter rather than the raw macro.

**Implementer note:** returning an error as a `*exec.Value` is how gonja surfaces failures from a `Macro`; verify how `exec.AsValue` of an `error` propagates and adjust if the error is swallowed. If it is, return a sentinel value and check it in Task 7's renderer. Confirm before assuming.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./engine/ -run TestAuthoredMacro -v`
Expected: PASS, and critically **the process must not exit** — an unguarded recursive macro aborts the test binary with `exit status 2` rather than failing a test.

- [ ] **Step 5: Commit**

```bash
git add engine/authored_macro.go engine/authored_macro_test.go
git commit -m "feat(engine): bound macro recursion depth"
```

---

### Task 4: Include and import depth counter

**Files:**
- Create: `engine/authored_include.go`
- Test: `engine/authored_include_test.go`

**Interfaces:**
- Produces: `installIncludeGuard(map[string]parser.ControlStructureParser)`, `var ErrIncludeTooDeep`

- [ ] **Step 1: Write the failing test**

Create `engine/authored_include_test.go` covering: a legitimate include renders; self-inclusion (`/a` includes `/a`) returns `ErrIncludeTooDeep`; mutual inclusion (`/a`→`/b`→`/a`) returns it; a **runtime-expression** filename (`{% include target %}` where `target` is a variable) is still caught; and depth resets between renders. Use a partial set supplied through the loader.

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./engine/ -run TestAuthoredInclude -v`
Expected: FAIL — `undefined: engine.ErrIncludeTooDeep`.

- [ ] **Step 3: Implement**

Same wrapper shape as Task 3, with one critical difference: **the counter must be a pointer stored in the Context**, not a local.

```go
type depthBox struct{ n int }

const includeDepthKey = "__bpt_include_depth__"
```

`include` passes `r.Environment` — the same `*exec.Context` — into the nested template, but `Context.Inherit()` shadows writes, so a value written by the nested template does not propagate back. Only mutation through a **shared pointer** survives the boundary. Fetch-or-create the `*depthBox` from the context in `Execute`, increment, defer decrement, and reject above `maxDepth`.

A static pre-pass cannot replace this: `include` and `import` filenames are arbitrary runtime expressions. (`extends` is the exception — its filename is a string literal — which is why it is cut rather than wrapped: its recursion happens at parse time, before any `Execute` runs.)

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./engine/ -run TestAuthoredInclude -v`
Expected: PASS, process survives.

- [ ] **Step 5: Commit**

```bash
git add engine/authored_include.go engine/authored_include_test.go
git commit -m "feat(engine): bound include and import depth"
```

---

### Task 5: Tenant-scoped partial loader

**Files:**
- Create: `partials/registry.go`
- Test: `partials/registry_test.go`

**Interfaces:**
- Produces:
  - `type Partial struct{ Name, Content string }`
  - `type Set struct{ ... }`
  - `func NewSet(tenantID string, ps []Partial) (*Set, error)`
  - `func (s *Set) Loader(templateKey, templateSource string) (loaders.Loader, error)`
  - `func (s *Set) Fingerprint() string`
  - `func (s *Set) TenantID() string`

- [ ] **Step 1: Write the failing test**

Cover: bare names are rewritten to `/name` so authors never write the leading slash; the loader contains the template source plus exactly this tenant's partials and nothing else; `Fingerprint` is stable for identical content and **differs when content differs under the same names** (a name-only fingerprint collides); and two `Set`s with the same partial names but different tenants produce different fingerprints.

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./partials/ -v`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Implement**

`Loader` builds a `loaders.NewMemoryLoader` containing the template under `templateKey` plus `/`-prefixed partials. `Fingerprint` hashes tenant ID plus each partial's name **and content**, sorted for determinism.

**Fingerprint over content, not names.** A cached parse tree pins the partial content it was compiled against — `{% import %}` resolves at parse time — so editing a partial must invalidate the entries that depend on it. A name-only fingerprint would not change, and the stale tree would render forever.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./partials/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add partials/
git commit -m "feat(partials): tenant-scoped partial registry and loader"
```

---

### Task 6: Tenant-aware template cache

**This task fixes a verified cross-tenant data leak that fails open.** It is the highest-severity item in this plan.

**Files:**
- Create: `engine/cache.go`
- Test: `engine/cache_test.go`

**Interfaces:**
- Produces:
  - `type CacheKey struct{ TenantID string; PromptID string; Version int; PartialFingerprint string }`
  - `type Cache struct{ ... }`
  - `func NewCache(max int) *Cache`
  - `func (c *Cache) GetOrCompile(k CacheKey, compile func() (*exec.Template, error)) (*exec.Template, error)`

- [ ] **Step 1: Write the failing test**

The load-bearing test:

```go
// exec.NewTemplate pins the loader on the Template (exec/template.go:43) and
// Execute renders with that compile-time t.loader (:71). A per-tenant loader
// built at request time is therefore IGNORED on a cache hit. Verified: without
// the tenant in the key, tenant B receives tenant A's partial content, with no
// error raised — it fails open.
func TestCacheKeyIsolatesTenants(t *testing.T) {
	// Compile the same prompt id + version for tenant A, whose partial says
	// "ACME-CONFIDENTIAL", then for tenant B, whose partial says "B-CONTENT".
	// Assert tenant B's render contains B-CONTENT and NOT ACME-CONFIDENTIAL.
}
```

Also cover: identical keys hit the cache (compile called once); a changed partial fingerprint misses; a changed resolved version misses; and concurrent `GetOrCompile` for the same key is safe under `-race`.

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./engine/ -run TestCache -v`
Expected: FAIL — `undefined: engine.NewCache`.

- [ ] **Step 3: Implement**

A mutex-guarded map keyed on the `CacheKey` struct, with a simple size bound.

**Key on the RESOLVED version, never the requested one.** With no `x-bf-prompt-version` header the resolver returns the prompt's latest version; keying on the requested version — absent, i.e. 0 — serves the stale compiled template forever after a new version is published.

A shared `*exec.Template` is safe for concurrent `Execute`: it builds a fresh `Environment` with `Context.Inherit()` per call, and `{% set %}` does not leak across renders. No per-request compilation or locking of the template itself is needed.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./engine/ -run TestCache -race -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add engine/cache.go engine/cache_test.go
git commit -m "feat(engine): tenant-aware template cache"
```

---

### Task 7: The authored renderer

**Files:**
- Create: `engine/authored.go`
- Test: `engine/authored_test.go`

**Interfaces:**
- Consumes: everything above, plus `Budget`, `GuardExpressions`, `GuardExpressionNodes`, `CheckIterables`, `AsMissingVariables`.
- Produces:
  - `type Authored struct{ ... }`
  - `func NewAuthored(limits Limits, cache *Cache) *Authored`
  - `func (a *Authored) Render(set *partials.Set, key CacheKey, src string, vars map[string]any, budget *Budget) (string, error)`

Unlike `Untrusted.Render`, this **returns an error** rather than falling back. Delete the `renderAuthored` stub from Task 3's test file and point it here.

- [ ] **Step 1: Write the failing test**

Cover, in addition to the guards already tested: a missing variable produces an error for which `AsMissingVariables` returns the names (the 400 path); the panic barrier catches a panicking template rather than exiting; the output budget is enforced; `GuardExpressions` and `GuardExpressionNodes` are both applied with `AuthoredLimits`; and a full realistic template with a macro, an include and a nested loop renders correctly end to end.

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./engine/ -run TestAuthoredRender -v`
Expected: FAIL — `undefined: engine.NewAuthored`.

- [ ] **Step 3: Implement**

Order of operations, mirroring `Untrusted.Render` and for the same reasons:

1. `len(src) > MaxTemplateBytes` → error.
2. `GuardExpressions` — **before** `exec.NewTemplate`; the parser is not recursion-safe and its overflow is fatal.
3. `CheckIterables` on the variable map.
4. `cache.GetOrCompile` with the tenant-bearing key; the compile func builds the loader from `set` and calls `exec.NewTemplate`.
5. `AuthoredGuardLoops` and `GuardExpressionNodes` on the parse tree.
6. Execute inside the panic barrier, writing through `budget.Writer`.

Steps 5 and 6 run on **every** render, not only on a cache miss — a cached tree still needs its guards applied, and `budget` is per-request.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./... -race -v`
Expected: PASS, including all 109 tests from Plan 1.

- [ ] **Step 5: Commit**

```bash
git add engine/authored.go engine/authored_test.go
git commit -m "feat(engine): authored renderer"
```

---

## Out of scope for this plan

- **`extends` via literal-graph cycle detection.** Viable — filenames are string literals — but a separate piece of work. Until then `extends` stays cut.
- **A runtime step counter or wall-clock deadline for authored loops.** `AuthoredLimits.MaxLoopDepth` of 4 is an expressiveness decision, not a safety bound; the honest controls belong with the plugin layer that owns the goroutine (Plan 3).
- Plugin wiring, `/render`, partial CRUD, and UI — Plans 3–5.

## Self-Review

**Spec coverage.** Authored control-structure set and the four cuts → Task 1. `AuthoredGuardLoops` with `Blocks`/`Macros` roots → Task 2. Macro depth counter → Task 3. Include/import depth → Task 4. Tenant-scoped partials → Task 5. Tenant-aware cache key → Task 6. Authored renderer, strict errors, panic barrier → Task 7.

**Type consistency.** `Limits` gains `MaxMacroDepth` and `MaxIncludeDepth` in Task 1 and both are used in Tasks 3, 4 and 7. `CacheKey` is defined in Task 6 and consumed in Task 7. `partials.Set` is defined in Task 5 and consumed in Tasks 6 and 7.

**Verified before writing, not assumed.** `MacroControlStructure` embeds the exported `*nodes.Macro`; `exec.MacroNodeToFunc(node, r) (Macro, error)` and `exec.Macro = func(*VarArgs) *Value` are exported; `MacroControlStructure.Execute(r, tag)` calls `Context.Set(name, macro)`; `parser.ControlStructureParser = func(*Parser, *Parser) (nodes.ControlStructure, error)`; `nodes.Macro.Wrapper` and `nodes.Template.{Blocks,Macros}` are exported. Task 2's remaining concrete type names are explicitly flagged for the implementer to confirm rather than guess — the mistake that broke the untrusted walker twice, and that produced a call to a `builtins.FilterNames()` which does not exist.
