# Prompt Templates Engine — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the sandboxed Jinja2 render engine for the Bifrost prompt-templates plugin, as a standalone Go module with no Bifrost dependency.

**Architecture:** Two isolated gonja environments. The *authored* environment renders org-written prompt templates with full Jinja2 and depth-guarded partial inclusion. The *untrusted* environment renders end-user chat messages with a deliberately tiny control-structure set, an isolated loader, and an allowlisted variable map; anything it cannot render safely falls back to the original message byte-for-byte. Safety rests on constructs failing at **parse** time, not on runtime caps.

**Tech Stack:** Go 1.26.6, `github.com/nikolalohinski/gonja/v2` v2.9.0, `stretchr/testify`.

**Spec:** `docs/superpowers/specs/2026-08-25-prompt-templates-design.md`

**Repo:** `arbi-ai/bifrost-prompt-templates` (new, currently empty). All paths below are relative to that repo's root, **not** the Bifrost fork.

## Global Constraints

- Go pinned to **1.26.6** in `go.mod`. Not 1.26.1 — the Bifrost plugin docs say 1.26.1 and are stale; `core/go.mod` is on 1.26.6 and the `.so` ABI needs an exact match.
- `gonja.FromString` is **banned repo-wide**. It attaches a filesystem loader rooted at the working directory. Task 1 adds a test that enforces this.
- No import of `github.com/maximhq/bifrost/*` anywhere in this module. The engine stays Bifrost-agnostic; the plugin layer (a later plan) adapts it.
- Every render goes through `exec.NewTemplate` with an explicit `loaders.NewMemoryLoader`. Memory-loader keys **must** begin with `/`.
- Untrusted-path failures never propagate as errors to the caller. They return the original input plus an outcome record.

**The untrusted control-structure set is `if`, `for`, `raw` — nothing else.** `set` is excluded in both forms (they share one `setParser` and the block-form marker `cs.body` is unexported, so the safe assignment form cannot be admitted alone). `with` is excluded because `WithControlStructure`'s fields are all unexported, so the loop-depth guard cannot descend into its body and a loop nest hidden inside it would go uncounted.

**Parse-time rejection is necessary but not sufficient.** Two verified attack classes never reach the control-structure layer: the parser itself overflows the stack on deeply nested expressions (Task 4), and a single `{{ … }}` can allocate gigabytes through sizing filters (Task 5). Both run before or outside every control-structure guard, so they are separate tasks ahead of the AST walk.

---

### Task 1: Module scaffold and the banned-API guard

**Files:**
- Create: `go.mod`, `.gitignore`
- Create: `engine/doc.go`
- Test: `engine/banned_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: module path `github.com/arbi-ai/bifrost-prompt-templates`; package `engine`.

- [ ] **Step 1: Initialise the module**

```bash
git init
go mod init github.com/arbi-ai/bifrost-prompt-templates
go mod edit -go=1.26.6
go get github.com/nikolalohinski/gonja/v2@v2.9.0
go get github.com/stretchr/testify@latest
mkdir -p engine
printf 'coverage.out\n*.so\n' > .gitignore
```

- [ ] **Step 2: Write the failing guard test**

Create `engine/banned_test.go`:

```go
package engine_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// gonja.FromString attaches loaders.MustNewFileSystemLoader("") — a filesystem
// loader rooted at the process working directory. Any use of it makes every
// sandbox guarantee in this module false. It must never appear in the tree.
func TestFromStringIsNeverUsed(t *testing.T) {
	root, err := filepath.Abs("..")
	require.NoError(t, err)

	var offenders []string
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "testdata") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		if strings.HasSuffix(path, "banned_test.go") {
			return nil
		}
		src, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if strings.Contains(string(src), "gonja.FromString") {
			offenders = append(offenders, path)
		}
		return nil
	})
	require.NoError(t, err)
	require.Empty(t, offenders, "gonja.FromString is banned; use exec.NewTemplate with a memory loader")
}
```

- [ ] **Step 3: Run it to verify it passes on an empty tree**

Run: `go test ./engine/ -run TestFromStringIsNeverUsed -v`
Expected: PASS (no `.go` files contain the string yet). This test is a ratchet — it guards every later task.

- [ ] **Step 4: Add the package doc file**

Create `engine/doc.go`:

```go
// Package engine renders Jinja2 templates in two isolated environments.
//
// The authored environment runs org-written prompt templates with full Jinja2
// and depth-guarded partial inclusion. The untrusted environment runs end-user
// text with a minimal control-structure set and an isolated loader.
//
// Safety comes from constructs failing at parse time, not from runtime caps:
// gonja's output writer can be swapped out by buffering control structures, and
// a recursive macro causes a fatal stack overflow that recover() cannot catch.
package engine
```

- [ ] **Step 5: Commit**

```bash
git add go.mod go.sum .gitignore engine/
git commit -m "chore: module scaffold and gonja.FromString ban guard"
```

---

### Task 2: The restricted control-structure set

This is the single most important task in the plan. Every DoS and exfiltration vector in the spec is closed here, at parse time.

**Files:**
- Create: `engine/controlstructures.go`
- Test: `engine/controlstructures_test.go`

**Interfaces:**
- Consumes: Task 1's `engine` package.
- Produces:
  - `func UntrustedControlStructures() *exec.ControlStructureSet`
  - `var UntrustedAllowed = []string{"if", "for", "raw", "with"}`

- [ ] **Step 1: Write the failing test**

Create `engine/controlstructures_test.go`:

```go
package engine_test

import (
	"testing"

	"github.com/arbi-ai/bifrost-prompt-templates/engine"
	"github.com/nikolalohinski/gonja/v2/config"
	"github.com/nikolalohinski/gonja/v2/exec"
	"github.com/nikolalohinski/gonja/v2/loaders"
	"github.com/stretchr/testify/require"
)

// parseUntrusted parses src in the untrusted environment and reports the parse error, if any.
func parseUntrusted(t *testing.T, src string) error {
	t.Helper()
	ldr, err := loaders.NewMemoryLoader(map[string]string{"/__msg__": src})
	require.NoError(t, err)
	env := &exec.Environment{
		Context:           exec.NewContext(map[string]any{}),
		Filters:           gonjaFilters(),
		Tests:             gonjaTests(),
		ControlStructures: engine.UntrustedControlStructures(),
		Methods:           gonjaMethods(),
	}
	_, err = exec.NewTemplate("/__msg__", config.New(), ldr, env)
	return err
}

// Each of these is a verified attack from the design spec. They must fail at
// PARSE — not be caught at runtime, because two of them cannot be caught at all.
func TestDangerousControlStructuresRejectedAtParse(t *testing.T) {
	cases := map[string]string{
		// Recursive macro => Go stack overflow => process exit, recover() cannot catch it.
		"macro":     `{% macro f(n) %}{{ f(n+1) }}{% endmacro %}{% set _ = f(0) %}`,
		"call":      `{% call f() %}x{% endcall %}`,
		// These swap sub.Output for an in-memory buffer, bypassing the output cap.
		"filter":    `{% filter upper %}x{% endfilter %}`,
		"set_block": `{% set x %}y{% endset %}`,
		// These read arbitrary partials and recurse without bound.
		"include":   `{% include '/other' %}`,
		"extends":   `{% extends '/other' %}`,
		"import":    `{% import '/other' as o %}`,
		"from":      `{% from '/other' import thing %}`,
		"block":     `{% block b %}x{% endblock %}`,
		// Assignment-set is dropped too: it shares setParser with the block form.
		"set_assign": `{% set x = 1 %}`,
		// with is harmless in itself, but WithControlStructure's fields are all
		// unexported, so GuardLoops cannot descend into its body and a loop nest
		// hidden inside it would be counted as depth zero.
		"with": `{% with y = 1 %}{{ y }}{% endwith %}`,
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			require.Error(t, parseUntrusted(t, src), "must be rejected at parse time")
		})
	}
}

// The restriction must not cost the expressiveness the spec's goals ask for.
func TestAllowedControlStructuresStillParse(t *testing.T) {
	cases := map[string]string{
		"if":       `{% if tier == 'pro' %}PRO{% else %}FREE{% endif %}`,
		"elif":     `{% if a %}A{% elif b %}B{% else %}C{% endif %}`,
		"for":      `{% for o in orders %}[{{ o.id }}]{% endfor %}`,
		"for_else": `{% for o in orders %}{{ o.id }}{% else %}none{% endfor %}`,
		"for_if":   `{% for o in orders if o.ok %}{{ o.id }}{% endfor %}`,
		"loop_var": `{% for o in orders %}{{ loop.index }}{% endfor %}`,
		"raw":      `{% raw %}{{ literal }}{% endraw %}`,
		"var":      `Hi {{ name }}`,
		"nested":   `{{ user.profile.city }}`,
		"filter":   `{{ name | upper }}`,
		"default":  `{{ missing | default('x') }}`,
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			require.NoError(t, parseUntrusted(t, src))
		})
	}
}
```

Add the small helpers in the same file (they exist so the test does not depend on
the production environment builder, which arrives in Task 6):

```go
func gonjaFilters() *exec.FilterSet { return builtins.Filters }
func gonjaTests() *exec.TestSet     { return builtins.Tests }
func gonjaMethods() exec.Methods    { return builtins.Methods }
```

with `"github.com/nikolalohinski/gonja/v2/builtins"` added to the imports.

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./engine/ -run TestDangerousControlStructures -v`
Expected: FAIL — `undefined: engine.UntrustedControlStructures`.

- [ ] **Step 3: Implement the restricted set**

Create `engine/controlstructures.go`:

```go
package engine

import (
	"github.com/nikolalohinski/gonja/v2/builtins"
	"github.com/nikolalohinski/gonja/v2/exec"
	"github.com/nikolalohinski/gonja/v2/parser"
)

// UntrustedAllowed is the complete set of control structures permitted when
// rendering end-user text. Everything absent from this list is a parse error.
//
// Deliberately excluded, each for a verified reason:
//
//	macro, call        recursive macro => fatal stack overflow, unrecoverable
//	filter, set        swap sub.Output for a buffer, bypassing the output cap
//	include, extends,
//	import, from       read arbitrary partials; unbounded recursion
//	block              template inheritance plumbing, no use in end-user text
//	with               all its node's fields are unexported, so GuardLoops cannot
//	                   descend into its body; a loop nest inside it is uncounted
//
// set is excluded in both its forms: they share one parser and the block-form
// marker is unexported, so the safe assignment form cannot be admitted alone.
var UntrustedAllowed = []string{"if", "for", "raw"}

// UntrustedControlStructures builds a control-structure set containing only
// UntrustedAllowed, copied out of gonja's builtins.
//
// The set is built by subtraction rather than construction because the builtin
// parser functions (forParser, ifParser, ...) are unexported; ControlStructureSet.Get
// is the only way to reach them.
func UntrustedControlStructures() *exec.ControlStructureSet {
	allowed := make(map[string]parser.ControlStructureParser, len(UntrustedAllowed))
	for _, name := range UntrustedAllowed {
		if p, ok := builtins.ControlStructures.Get(name); ok {
			allowed[name] = p
		}
	}
	return exec.NewControlStructureSet(allowed)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./engine/ -v`
Expected: PASS, including every subtest of `TestDangerousControlStructuresRejectedAtParse`.

- [ ] **Step 5: Commit**

```bash
git add engine/controlstructures.go engine/controlstructures_test.go
git commit -m "feat(engine): restricted control-structure set for untrusted templates"
```

---

### Task 3: Per-request output cap

**Files:**
- Create: `engine/capwriter.go`
- Test: `engine/capwriter_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces:
  - `type Budget struct{ ... }`
  - `func NewBudget(maxBytes int64) *Budget`
  - `func (b *Budget) Writer(w io.Writer) io.Writer`
  - `var ErrOutputCapExceeded = errors.New("render output cap exceeded")`

The budget is per **request**, shared across every message rendered for that request — not per message, or N messages multiply it.

- [ ] **Step 1: Write the failing test**

Create `engine/capwriter_test.go`:

```go
package engine_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/arbi-ai/bifrost-prompt-templates/engine"
	"github.com/stretchr/testify/require"
)

func TestBudgetAllowsWritesUpToCap(t *testing.T) {
	b := engine.NewBudget(10)
	var out bytes.Buffer
	n, err := b.Writer(&out).Write([]byte("0123456789"))
	require.NoError(t, err)
	require.Equal(t, 10, n)
	require.Equal(t, "0123456789", out.String())
}

func TestBudgetTripsAtBoundary(t *testing.T) {
	b := engine.NewBudget(10)
	var out bytes.Buffer
	_, err := b.Writer(&out).Write([]byte("01234567890"))
	require.ErrorIs(t, err, engine.ErrOutputCapExceeded)
}

// The budget is per request: a second message cannot start from a fresh cap.
func TestBudgetIsSharedAcrossWriters(t *testing.T) {
	b := engine.NewBudget(10)
	var first, second bytes.Buffer

	_, err := b.Writer(&first).Write([]byte("01234"))
	require.NoError(t, err)

	_, err = b.Writer(&second).Write([]byte("56789X"))
	require.ErrorIs(t, err, engine.ErrOutputCapExceeded)
}

func TestBudgetRejectsLargeSingleWrite(t *testing.T) {
	b := engine.NewBudget(1024)
	var out bytes.Buffer
	_, err := b.Writer(&out).Write([]byte(strings.Repeat("x", 4096)))
	require.ErrorIs(t, err, engine.ErrOutputCapExceeded)
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./engine/ -run TestBudget -v`
Expected: FAIL — `undefined: engine.NewBudget`.

- [ ] **Step 3: Implement the budget**

Create `engine/capwriter.go`:

```go
package engine

import (
	"errors"
	"io"
	"sync"
)

// ErrOutputCapExceeded is returned by a budgeted writer once the request's
// total rendered output would exceed its cap. Returning an error from Write
// aborts gonja's render synchronously.
var ErrOutputCapExceeded = errors.New("render output cap exceeded")

// Budget is a per-request output allowance shared by every writer it hands out.
//
// It is a secondary control, not a general backstop: gonja's buffering control
// structures ({% filter %}, {% macro %}, {% set %}block, {% call %}) redirect
// rendering into their own in-memory buffer and never touch this writer. Those
// constructs are excluded at parse time instead — see UntrustedAllowed.
type Budget struct {
	mu        sync.Mutex
	remaining int64
}

func NewBudget(maxBytes int64) *Budget {
	return &Budget{remaining: maxBytes}
}

// Writer wraps w so writes draw down the shared budget.
func (b *Budget) Writer(w io.Writer) io.Writer {
	return &budgetedWriter{budget: b, inner: w}
}

// take reserves n bytes, reporting whether the budget allowed it.
//
// A refusal zeroes the remaining budget rather than leaving it partially
// spent: once a request has tried to exceed its allowance, later writes for
// that same request should fail fast rather than trickle through.
func (b *Budget) take(n int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.remaining < n {
		b.remaining = 0
		return false
	}
	b.remaining -= n
	return true
}

type budgetedWriter struct {
	budget *Budget
	inner  io.Writer
}

func (w *budgetedWriter) Write(p []byte) (int, error) {
	if !w.budget.take(int64(len(p))) {
		return 0, ErrOutputCapExceeded
	}
	return w.inner.Write(p)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./engine/ -run TestBudget -v`
Expected: PASS (4 tests).

- [ ] **Step 5: Commit**

```bash
git add engine/capwriter.go engine/capwriter_test.go
git commit -m "feat(engine): per-request output budget"
```

---

### Task 4: AST guard for loop depth and recursive loops

The output cap cannot see a loop that emits nothing. Nested loops multiply, and `{% for ... recursive %}` is unbounded on its own. Both are caught by walking the parse tree before executing it.

**Files:**
- Create: `engine/astguard.go`
- Test: `engine/astguard_test.go`

**Interfaces:**
- Consumes: Task 2's `UntrustedControlStructures`.
- Produces:
  - `func GuardLoops(root *nodes.Template, maxDepth int) error`
  - `var ErrLoopTooDeep = errors.New("template exceeds maximum loop nesting depth")`
  - `var ErrRecursiveLoop = errors.New("recursive for loops are not permitted")`

- [ ] **Step 1: Write the failing test**

Create `engine/astguard_test.go`:

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

func parseTreeForGuard(t *testing.T, src string) *exec.Template {
	t.Helper()
	ldr, err := loaders.NewMemoryLoader(map[string]string{"/__msg__": src})
	require.NoError(t, err)
	env := &exec.Environment{
		Context:           exec.NewContext(map[string]any{}),
		Filters:           builtins.Filters,
		Tests:             builtins.Tests,
		ControlStructures: engine.UntrustedControlStructures(),
		Methods:           builtins.Methods,
	}
	tpl, err := exec.NewTemplate("/__msg__", config.New(), ldr, env)
	require.NoError(t, err)
	return tpl
}

func TestGuardAllowsShallowLoops(t *testing.T) {
	tpl := parseTreeForGuard(t, `{% for a in xs %}{% for b in ys %}{{ b }}{% endfor %}{% endfor %}`)
	require.NoError(t, engine.GuardLoops(tpl.Root(), 2))
}

// Nested loops multiply: three loops over a 1000-element array is 10^9 iterations
// emitting nothing at all, so the output cap never fires.
func TestGuardRejectsDeepLoops(t *testing.T) {
	tpl := parseTreeForGuard(t, `{% for a in xs %}{% for b in xs %}{% for c in xs %}{% endfor %}{% endfor %}{% endfor %}`)
	require.ErrorIs(t, engine.GuardLoops(tpl.Root(), 2), engine.ErrLoopTooDeep)
}

func TestGuardRejectsRecursiveLoop(t *testing.T) {
	tpl := parseTreeForGuard(t, `{% for a in xs recursive %}{{ a }}{% endfor %}`)
	require.ErrorIs(t, engine.GuardLoops(tpl.Root(), 2), engine.ErrRecursiveLoop)
}

// A reflective walk over exported fields misses these entirely: IfControlStructure
// holds its branches in Wrappers []*nodes.Wrapper. Executed unguarded, each of
// these allocates ~25 GB.
func TestGuardWalksIntoIfBranches(t *testing.T) {
	deep := `{% for a in xs %}{% for b in xs %}{% for d in xs %}{% endfor %}{% endfor %}{% endfor %}`
	cases := map[string]string{
		"if":   `{% if c %}` + deep + `{% endif %}`,
		"else": `{% if c %}x{% else %}` + deep + `{% endif %}`,
		"elif": `{% if c %}x{% elif d %}` + deep + `{% endif %}`,
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			tpl := parseTreeForGuard(t, src)
			require.ErrorIs(t, engine.GuardLoops(tpl.Root(), 2), engine.ErrLoopTooDeep)
		})
	}
}

// A for loop's {% else %} arm is not nested inside the loop, so it must be
// walked at the loop's own depth. Counting it as nested would reject valid input.
func TestGuardDoesNotCountForElseAsNested(t *testing.T) {
	tpl := parseTreeForGuard(t, `{% for a in xs %}{% endfor %}`)
	require.NoError(t, engine.GuardLoops(tpl.Root(), 1))

	tpl = parseTreeForGuard(t, `{% for a in xs %}x{% else %}{% for b in ys %}{% endfor %}{% endfor %}`)
	require.NoError(t, engine.GuardLoops(tpl.Root(), 1))
}

// The guard must fail closed. An unrecognised node type could hide a loop nest,
// so it is rejected rather than walked past.
func TestGuardFailsClosedOnUnknownNode(t *testing.T) {
	require.ErrorIs(t, engine.GuardLoops(&nodes.Template{
		Nodes: []nodes.Node{&controlStructures.WithControlStructure{}},
	}, 2), engine.ErrUnwalkableNode)
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./engine/ -run TestGuard -v`
Expected: FAIL — `undefined: engine.GuardLoops`.

- [ ] **Step 3: Implement the guard**

Create `engine/astguard.go`:

```go
package engine

import (
	"errors"

	controlStructures "github.com/nikolalohinski/gonja/v2/builtins/control_structures"
	"github.com/nikolalohinski/gonja/v2/nodes"
)

var (
	// ErrLoopTooDeep reports nested loops beyond the permitted depth. Nesting
	// multiplies, and a non-emitting loop is invisible to the output budget.
	ErrLoopTooDeep = errors.New("template exceeds maximum loop nesting depth")

	// ErrRecursiveLoop reports {% for ... recursive %}, which is unbounded.
	ErrRecursiveLoop = errors.New("recursive for loops are not permitted")
)

// ErrUnwalkableNode reports a node type the guard cannot descend into. It is an
// error rather than a skip: a node whose children are invisible could hide a
// loop nest, so the guard must fail closed.
var ErrUnwalkableNode = errors.New("template contains a node the loop guard cannot inspect")

// GuardLoops walks a parsed template and rejects loop nesting deeper than
// maxDepth, any recursive loop, and any node type it cannot inspect.
//
// This is an explicit type switch, NOT reflection over exported fields. A
// reflective walk silently under-counts: IfControlStructure holds its branches
// in Wrappers []*nodes.Wrapper, and WithControlStructure holds everything in
// unexported fields, so a three-deep loop nest inside {% if %} or {% with %}
// was counted as depth zero and executed, allocating 25 GB.
//
// Failing closed is the point. Every node type reachable in UntrustedAllowed is
// handled below; anything else — a gonja upgrade adding a node, a control
// structure someone adds to the allowed set without updating this switch —
// returns ErrUnwalkableNode and falls back to verbatim, rather than being
// waved through uninspected.
func GuardLoops(root *nodes.Template, maxDepth int) error {
	if root == nil {
		return nil
	}
	return walkNodes(root.Nodes, 0, maxDepth)
}

func walkNodes(ns []nodes.Node, depth, maxDepth int) error {
	for _, n := range ns {
		if err := walkNode(n, depth, maxDepth); err != nil {
			return err
		}
	}
	return nil
}

func walkWrapper(w *nodes.Wrapper, depth, maxDepth int) error {
	if w == nil {
		return nil
	}
	return walkNodes(w.Nodes, depth, maxDepth)
}

func walkNode(n nodes.Node, depth, maxDepth int) error {
	switch typed := n.(type) {
	case nil:
		return nil

	// Leaves: carry no child nodes, so nothing can hide inside them.
	case *nodes.Output, *nodes.Data, *nodes.Comment:
		return nil

	case *controlStructures.ForControlStructure:
		if typed.Recursive {
			return ErrRecursiveLoop
		}
		depth++
		if depth > maxDepth {
			return ErrLoopTooDeep
		}
		if err := walkWrapper(typed.BodyWrapper, depth, maxDepth); err != nil {
			return err
		}
		// The {% else %} arm of a for loop is NOT nested inside the loop, so it
		// is walked at the loop's own depth, not the incremented one.
		return walkWrapper(typed.EmptyWrapper, depth-1, maxDepth)

	case *controlStructures.IfControlStructure:
		// Wrappers holds one entry per branch: the if body, each elif body, and
		// the else body. Branches are siblings, so each is walked at the same
		// depth — this is the case the reflective walker missed entirely.
		for _, wrapper := range typed.Wrappers {
			if err := walkWrapper(wrapper, depth, maxDepth); err != nil {
				return err
			}
		}
		return nil

	case *controlStructures.RawControlStructure:
		// Raw content is never parsed as template code.
		return nil

	default:
		return ErrUnwalkableNode
	}
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./engine/ -run TestGuard -v`
Expected: PASS. `TestGuardWalksIntoIfBranches` is the critical one — its three subtests are the exact case a reflective walker silently permits.

- [ ] **Step 5: Commit**

```bash
git add engine/astguard.go engine/astguard_test.go
git commit -m "feat(engine): reject deep and recursive loops at parse time"
```

---

### Task 5: Variable allowlist and iterable size cap

Depth guarding alone is not enough: `{% for %}` iterates client-supplied arrays from the request body, which no `range` cap touches. Bounding depth *and* iterable length bounds total iterations.

**Files:**
- Create: `engine/vars.go`
- Test: `engine/vars_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces:
  - `func Allowlist(vars map[string]any, allowed []string) map[string]any`
  - `func CheckIterables(vars map[string]any, maxLen int) error`
  - `var ErrIterableTooLarge = errors.New("variable contains an iterable larger than the permitted maximum")`

- [ ] **Step 1: Write the failing test**

Create `engine/vars_test.go`:

```go
package engine_test

import (
	"testing"

	"github.com/arbi-ai/bifrost-prompt-templates/engine"
	"github.com/stretchr/testify/require"
)

// The untrusted environment must never see the merged map: version defaults are
// org-authored content the caller never supplied and must not be able to read back.
func TestAllowlistDropsUndeclaredKeys(t *testing.T) {
	got := engine.Allowlist(
		map[string]any{"name": "Ada", "hidden_default": "ORG-CONFIDENTIAL"},
		[]string{"name"},
	)
	require.Equal(t, map[string]any{"name": "Ada"}, got)
}

func TestAllowlistOmitsMissingKeys(t *testing.T) {
	got := engine.Allowlist(map[string]any{"name": "Ada"}, []string{"name", "absent"})
	require.Equal(t, map[string]any{"name": "Ada"}, got)
	require.NotContains(t, got, "absent")
}

func TestCheckIterablesAcceptsSmallCollections(t *testing.T) {
	vars := map[string]any{"orders": []any{1, 2, 3}, "name": "Ada"}
	require.NoError(t, engine.CheckIterables(vars, 10))
}

func TestCheckIterablesRejectsLargeSlice(t *testing.T) {
	big := make([]any, 101)
	require.ErrorIs(t, engine.CheckIterables(map[string]any{"xs": big}, 100), engine.ErrIterableTooLarge)
}

func TestCheckIterablesRecursesIntoNestedStructures(t *testing.T) {
	big := make([]any, 101)
	vars := map[string]any{"outer": map[string]any{"inner": big}}
	require.ErrorIs(t, engine.CheckIterables(vars, 100), engine.ErrIterableTooLarge)
}

// gonja iterates strings character by character, so a long string IS an
// iteration hazard: {% for c in big %} over a 200KB string runs 200k times and
// allocates 345MB at loop depth 1 — inside max_loop_depth, needing no bypass.
// max_template_bytes does not bound it: that governs the template, not the
// variable map.
func TestCheckIterablesCountsStringLength(t *testing.T) {
	vars := map[string]any{"doc": strings.Repeat("x", 101)}
	require.ErrorIs(t, engine.CheckIterables(vars, 100), engine.ErrIterableTooLarge)
}

func TestCheckIterablesAllowsShortString(t *testing.T) {
	require.NoError(t, engine.CheckIterables(map[string]any{"name": "Ada"}, 100))
}

// JSON decoding yields []any and map[string]any, but variables can also arrive
// from a header parse as concrete Go slice types. Those must be counted too.
func TestCheckIterablesHandlesConcreteSliceTypes(t *testing.T) {
	vars := map[string]any{"tags": make([]string, 101)}
	require.ErrorIs(t, engine.CheckIterables(vars, 100), engine.ErrIterableTooLarge)
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./engine/ -run "TestAllowlist|TestCheckIterables" -v`
Expected: FAIL — `undefined: engine.Allowlist`.

- [ ] **Step 3: Implement**

Create `engine/vars.go`:

```go
package engine

import "errors"

// ErrIterableTooLarge reports a variable holding a collection longer than the
// permitted maximum. Combined with the loop-depth guard this bounds total
// iterations at maxLen^maxDepth.
var ErrIterableTooLarge = errors.New("variable contains an iterable larger than the permitted maximum")

// Allowlist returns the subset of vars whose keys appear in allowed.
//
// The untrusted environment receives this, never the merged variable map:
// version-declared defaults are org-authored and an end user must not be able
// to print them by guessing a name.
func Allowlist(vars map[string]any, allowed []string) map[string]any {
	out := make(map[string]any, len(allowed))
	for _, key := range allowed {
		if v, ok := vars[key]; ok {
			out[key] = v
		}
	}
	return out
}

// CheckIterables reports an error if any value reachable from vars can be
// iterated more than maxLen times.
//
// Strings count by length: gonja iterates them character by character, so a
// 200KB string variable is a 200 000-iteration loop at depth 1. Exempting them
// — as an earlier revision did — leaves the iteration bound false.
//
// Reflection is used rather than a type switch on []any/map[string]any because
// variables do not only arrive from JSON: a header parse or a caller-supplied
// map can hold concrete Go slice types, which a []any case silently misses.
// Strings get their own, much larger cap: a pasted document is a legitimate
// variable value, and a string's length costs output bytes (already governed by
// the output budget) as well as iterations. Collections get the tighter cap.
//
// With MaxLoopDepth of 1, the worst case is maxStringLen iterations of an empty
// loop body — bounded and cheap. It is the DEPTH limit, not the length limits,
// that makes this bound safe; see DefaultLimits.
func CheckIterables(vars map[string]any, maxLen, maxStringLen int) error {
	for _, v := range vars {
		if err := checkValue(reflect.ValueOf(v), maxLen, maxStringLen, 0); err != nil {
			return err
		}
	}
	return nil
}

// maxValueDepth bounds the recursion in checkValue itself, so a deeply nested
// variable structure cannot overflow the stack the way a deeply nested template
// expression can.
const maxValueDepth = 32

func checkValue(v reflect.Value, maxLen, maxStringLen, depth int) error {
	if depth > maxValueDepth {
		return ErrIterableTooLarge
	}
	for v.Kind() == reflect.Ptr || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return nil
		}
		v = v.Elem()
	}

	switch v.Kind() {
	case reflect.String:
		if v.Len() > maxStringLen {
			return ErrIterableTooLarge
		}
	case reflect.Slice, reflect.Array:
		if v.Len() > maxLen {
			return ErrIterableTooLarge
		}
		for i := 0; i < v.Len(); i++ {
			if err := checkValue(v.Index(i), maxLen, maxStringLen, depth+1); err != nil {
				return err
			}
		}
	case reflect.Map:
		if v.Len() > maxLen {
			return ErrIterableTooLarge
		}
		for _, key := range v.MapKeys() {
			if err := checkValue(v.MapIndex(key), maxLen, maxStringLen, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./engine/ -run "TestAllowlist|TestCheckIterables" -v`
Expected: PASS (6 tests).

- [ ] **Step 5: Commit**

```bash
git add engine/vars.go engine/vars_test.go
git commit -m "feat(engine): variable allowlist and iterable size cap"
```

---

### Task 5b: Pre-parse expression-depth guard

The single most severe finding in review. gonja's **parser** is not recursion-safe, and it runs on attacker input before every other guard in this plan.

**Files:**
- Create: `engine/depthguard.go`
- Test: `engine/depthguard_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `func GuardExpressionDepth(src string, maxDepth int) error`
  - `var ErrExpressionTooDeep = errors.New("template expression nesting is too deep")`

- [ ] **Step 1: Write the failing test**

Create `engine/depthguard_test.go`:

```go
package engine_test

import (
	"strings"
	"testing"

	"github.com/arbi-ai/bifrost-prompt-templates/engine"
	"github.com/stretchr/testify/require"
)

func TestDepthGuardAllowsOrdinaryTemplates(t *testing.T) {
	for _, src := range []string{
		`Hi {{ name }}`,
		`{{ (a + b) * (c - d) }}`,
		`{% for o in orders %}{{ o.items[0].price }}{% endfor %}`,
		`{{ user.profile.city | default('unknown') }}`,
	} {
		require.NoError(t, engine.GuardExpressionDepth(src, 64), src)
	}
}

// 60000 nested parens is 120007 bytes — under a 256KiB template cap — and causes
// a FATAL stack overflow inside exec.NewTemplate that recover() cannot catch.
// The guard must reject it without ever handing it to the parser.
func TestDepthGuardRejectsDeepNesting(t *testing.T) {
	for _, pair := range []struct{ open, close string }{
		{"(", ")"}, {"[", "]"}, {"{", "}"},
	} {
		src := "{{ " + strings.Repeat(pair.open, 500) + "1" + strings.Repeat(pair.close, 500) + " }}"
		require.ErrorIs(t, engine.GuardExpressionDepth(src, 64), engine.ErrExpressionTooDeep)
	}
}

// Depth must not accumulate across independent expressions.
func TestDepthGuardResetsBetweenExpressions(t *testing.T) {
	src := strings.Repeat("{{ ((a)) }} ", 1000)
	require.NoError(t, engine.GuardExpressionDepth(src, 64))
}

// Braces inside a raw block or literal text still count. The guard is a byte
// scan and deliberately does not try to understand context — over-rejecting an
// exotic message is acceptable; under-rejecting kills the process.
func TestDepthGuardIsConservative(t *testing.T) {
	src := "{{ " + strings.Repeat("(", 100) + " }}"
	require.ErrorIs(t, engine.GuardExpressionDepth(src, 64), engine.ErrExpressionTooDeep)
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./engine/ -run TestDepthGuard -v`
Expected: FAIL — `undefined: engine.GuardExpressionDepth`.

- [ ] **Step 3: Implement**

Create `engine/depthguard.go`:

```go
package engine

import "errors"

// ErrExpressionTooDeep reports bracket nesting beyond the permitted depth.
var ErrExpressionTooDeep = errors.New("template expression nesting is too deep")

// GuardExpressionDepth rejects source whose bracket nesting could overflow
// gonja's recursive-descent parser.
//
// This MUST run before exec.NewTemplate. The parser recurses per nesting level
// and overflows the goroutine stack fatally: 60 000 nested parens is 120 007
// bytes, passes any reasonable size cap, and exits the process with an error
// recover() cannot intercept. No post-parse guard can help, because the parse
// tree never exists.
//
// The scan is a single pass with three counters and does not attempt to
// understand string literals, comments or raw blocks. Over-rejecting an exotic
// message costs one verbatim fallback; under-rejecting costs the process.
func GuardExpressionDepth(src string, maxDepth int) error {
	var round, square, curly int
	for i := 0; i < len(src); i++ {
		switch src[i] {
		case '(':
			round++
			if round > maxDepth {
				return ErrExpressionTooDeep
			}
		case ')':
			if round > 0 {
				round--
			}
		case '[':
			square++
			if square > maxDepth {
				return ErrExpressionTooDeep
			}
		case ']':
			if square > 0 {
				square--
			}
		case '{':
			curly++
			if curly > maxDepth {
				return ErrExpressionTooDeep
			}
		case '}':
			if curly > 0 {
				curly--
			}
		}
	}
	return nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./engine/ -run TestDepthGuard -v`
Expected: PASS (4 tests).

- [ ] **Step 5: Commit**

```bash
git add engine/depthguard.go engine/depthguard_test.go
git commit -m "feat(engine): reject deeply nested expressions before parsing"
```

---

### Task 5c: Restricted filter set

A 27-byte template with no control structure allocates a gigabyte. The control-structure restriction does not touch this, because no control structure is involved.

**Files:**
- Create: `engine/filters.go`
- Test: `engine/filters_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `func UntrustedFilters() *exec.FilterSet`
  - `var UntrustedFilterDenylist = []string{...}`

- [ ] **Step 1: Write the failing test**

Create `engine/filters_test.go`:

```go
package engine_test

import (
	"testing"

	"github.com/arbi-ai/bifrost-prompt-templates/engine"
	"github.com/stretchr/testify/require"
)

// Each of these allocates proportionally to a caller-supplied integer, and the
// value is materialised in memory BEFORE the budgeted writer sees it — so the
// output cap, loop guard and iterable cap are all bypassed at once.
func TestUntrustedFiltersDropSizingFilters(t *testing.T) {
	fs := engine.UntrustedFilters()
	for _, name := range engine.UntrustedFilterDenylist {
		require.False(t, fs.Exists(name), "%s must not be available to untrusted templates", name)
	}
}

func TestUntrustedFiltersKeepOrdinaryFilters(t *testing.T) {
	fs := engine.UntrustedFilters()
	for _, name := range []string{"upper", "lower", "default", "join", "length", "trim"} {
		require.True(t, fs.Exists(name), "%s should remain available", name)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./engine/ -run TestUntrustedFilters -v`
Expected: FAIL — `undefined: engine.UntrustedFilters`.

- [ ] **Step 3: Implement**

Create `engine/filters.go`:

```go
package engine

import (
	"github.com/nikolalohinski/gonja/v2/builtins"
	"github.com/nikolalohinski/gonja/v2/exec"
)

// UntrustedFilterDenylist names filters unavailable to end-user text because
// they allocate proportionally to a caller-supplied numeric argument.
//
// Verified: {{ s | center(200000000) }} is 27 bytes and allocates 1091 MB,
// producing 200 MB of output that never reaches the budgeted writer, because
// the value is built in memory first.
var UntrustedFilterDenylist = []string{
	"center", "indent", "wordwrap", "truncate", "format",
	"filesizeformat", "batch", "slice", "list",
}

// UntrustedFilters returns the builtin filter set minus UntrustedFilterDenylist.
//
// Unlike control structures, filters are denylisted rather than allowlisted:
// the builtin set is large and mostly harmless string manipulation, and an
// allowlist would silently break ordinary templates on every gonja upgrade.
// The denylist is covered by a test that fails if a named filter disappears.
func UntrustedFilters() *exec.FilterSet {
	denied := make(map[string]struct{}, len(UntrustedFilterDenylist))
	for _, name := range UntrustedFilterDenylist {
		denied[name] = struct{}{}
	}

	allowed := make(map[string]exec.FilterFunction)
	for _, name := range builtins.FilterNames() {
		if _, blocked := denied[name]; blocked {
			continue
		}
		if fn, ok := builtins.Filters.Get(name); ok {
			allowed[name] = fn
		}
	}
	return exec.NewFilterSet(allowed)
}
```

**Implementer note:** `builtins.FilterNames()` may not exist. If it does not, enumerate names from the `builtins.Filters` map directly — check `exec.FilterSet` for an exported accessor mirroring `ControlStructureSet.Get`/`Exists`, and if the set exposes no enumeration at all, build `allowed` from an explicit list of the filters this project supports and document that upgrading gonja requires revisiting it. Do not fall back to `builtins.Filters` unmodified: that reintroduces the gigabyte allocation.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./engine/ -run TestUntrustedFilters -v`
Expected: PASS (2 tests).

- [ ] **Step 5: Commit**

```bash
git add engine/filters.go engine/filters_test.go
git commit -m "feat(engine): drop sizing filters from the untrusted environment"
```

---

### Task 6: The untrusted renderer

Assembles Tasks 2–5 into the render-or-return-verbatim path that runs on every client message.

**Files:**
- Create: `engine/untrusted.go`
- Test: `engine/untrusted_test.go`

**Interfaces:**
- Consumes: `UntrustedControlStructures`, `NewBudget`, `GuardLoops`, `Allowlist`, `CheckIterables`.
- Produces:
  - `type Limits struct{ MaxOutputBytes int64; MaxTemplateBytes int; MaxLoopDepth int; MaxIterableLen int }`
  - `func DefaultLimits() Limits`
  - `type Outcome struct{ Rendered bool; Err error }`
  - `type Untrusted struct{ ... }`
  - `func NewUntrusted(limits Limits) *Untrusted`
  - `func (u *Untrusted) Render(src string, vars map[string]any, allowed []string, budget *Budget) (string, Outcome)`

`Render` never returns an error. A failure returns `src` unchanged with `Outcome.Rendered == false`.

- [ ] **Step 1: Write the failing test**

Create `engine/untrusted_test.go`:

```go
package engine_test

import (
	"strings"
	"testing"

	"github.com/arbi-ai/bifrost-prompt-templates/engine"
	"github.com/stretchr/testify/require"
)

func renderUntrusted(t *testing.T, src string, vars map[string]any, allowed []string) (string, engine.Outcome) {
	t.Helper()
	u := engine.NewUntrusted(engine.DefaultLimits())
	return u.Render(src, vars, allowed, engine.NewBudget(engine.DefaultLimits().MaxOutputBytes))
}

func TestUntrustedRendersSuppliedVariables(t *testing.T) {
	out, oc := renderUntrusted(t, "Hi {{ name }}", map[string]any{"name": "Ada"}, []string{"name"})
	require.True(t, oc.Rendered)
	require.Equal(t, "Hi Ada", out)
}

func TestUntrustedRendersLoopsAndConditionals(t *testing.T) {
	src := `{% if pro %}PRO {% endif %}{% for o in orders %}[{{ o }}]{% endfor %}`
	vars := map[string]any{"pro": true, "orders": []any{"a", "b"}}
	out, oc := renderUntrusted(t, src, vars, []string{"pro", "orders"})
	require.True(t, oc.Rendered)
	require.Equal(t, "PRO [a][b]", out)
}

// An end user typing {{lol}} must not 400 production traffic, and must not have
// their text silently emptied — it comes back exactly as sent.
func TestUntrustedFallsBackVerbatimOnUndefined(t *testing.T) {
	const src = "what does {{ lol }} mean?"
	out, oc := renderUntrusted(t, src, map[string]any{}, []string{})
	require.False(t, oc.Rendered)
	require.Error(t, oc.Err)
	require.Equal(t, src, out)
}

func TestUntrustedFallsBackVerbatimOnParseError(t *testing.T) {
	const src = "broken {% if %} tag"
	out, oc := renderUntrusted(t, src, map[string]any{}, []string{})
	require.False(t, oc.Rendered)
	require.Equal(t, src, out)
}

// The renderer writes incrementally, so a mid-render failure has already emitted
// bytes. Returning that truncated text would be worse than not rendering at all.
func TestUntrustedDiscardsPartialOutput(t *testing.T) {
	// "LEAK-" is emitted before the undefined variable is evaluated, so a naive
	// implementation returning the buffer would yield "LEAK-" instead of src.
	const src = "LEAK-{{ missing }}"
	out, oc := renderUntrusted(t, src, map[string]any{}, []string{})
	require.False(t, oc.Rendered)
	require.Equal(t, src, out)
	require.NotEqual(t, "LEAK-", out, "partial output must be discarded, not returned")
}

// Every attack from the spec must come back verbatim rather than executing.
func TestUntrustedNeutralisesKnownAttacks(t *testing.T) {
	attacks := []string{
		`{% macro f(n) %}{{ f(n+1) }}{% endmacro %}{% set _ = f(0) %}`,
		`{% filter upper %}x{% endfilter %}`,
		`{% include '/__msg__' %}`,
		`{% extends '/__msg__' %}`,
		`{% set x %}y{% endset %}`,
	}
	for _, src := range attacks {
		out, oc := renderUntrusted(t, src, map[string]any{}, []string{})
		require.False(t, oc.Rendered, "must not execute: %s", src)
		require.Equal(t, src, out)
	}
}

// A variable VALUE containing template syntax must not be re-parsed.
func TestUntrustedDoesNotReparseVariableValues(t *testing.T) {
	vars := map[string]any{"payload": `{% include '/secret' %}`}
	out, oc := renderUntrusted(t, "user said: {{ payload }}", vars, []string{"payload"})
	require.True(t, oc.Rendered)
	require.Equal(t, `user said: {% include '/secret' %}`, out)
}

func TestUntrustedRejectsOversizedTemplate(t *testing.T) {
	src := strings.Repeat("x", engine.DefaultLimits().MaxTemplateBytes+1)
	out, oc := renderUntrusted(t, src, map[string]any{}, []string{})
	require.False(t, oc.Rendered)
	require.Equal(t, src, out)
}

func TestUntrustedRejectsOversizedIterable(t *testing.T) {
	big := make([]any, engine.DefaultLimits().MaxIterableLen+1)
	src := `{% for x in xs %}.{% endfor %}`
	out, oc := renderUntrusted(t, src, map[string]any{"xs": big}, []string{"xs"})
	require.False(t, oc.Rendered)
	require.Equal(t, src, out)
}

func TestUntrustedCannotReadUnallowlistedVariable(t *testing.T) {
	vars := map[string]any{"hidden_default": "ORG-CONFIDENTIAL"}
	const src = "stolen={{ hidden_default }}"
	out, oc := renderUntrusted(t, src, vars, []string{})
	require.False(t, oc.Rendered)
	require.Equal(t, src, out)
	require.NotContains(t, out, "ORG-CONFIDENTIAL")
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./engine/ -run TestUntrusted -v`
Expected: FAIL — `undefined: engine.NewUntrusted`.

- [ ] **Step 3: Implement the untrusted renderer**

Create `engine/untrusted.go`:

```go
package engine

import (
	"bytes"

	"github.com/nikolalohinski/gonja/v2/builtins"
	"github.com/nikolalohinski/gonja/v2/config"
	"github.com/nikolalohinski/gonja/v2/exec"
	"github.com/nikolalohinski/gonja/v2/loaders"
)

// untrustedKey is the memory-loader key holding the message being rendered.
// Memory-loader keys must begin with '/'.
const untrustedKey = "/__msg__"

// Limits bounds a single render. Zero values are not meaningful; use DefaultLimits.
type Limits struct {
	MaxOutputBytes     int64
	MaxTemplateBytes   int
	MaxExpressionDepth int
	MaxLoopDepth       int
	MaxIterableLen     int
	MaxStringLen       int
}

func DefaultLimits() Limits {
	return Limits{
		MaxOutputBytes:   1 << 20, // 1 MiB, per request
		MaxTemplateBytes: 64 << 10,
		// Bounds parser recursion. 120KB of nested parens — under the old 256KiB
		// template limit — caused a fatal stack overflow inside exec.NewTemplate.
		MaxExpressionDepth: 64,
		// Depth 1: no nested loops in end-user text. This is what makes the
		// iteration bound safe. At depth 2 a long string variable yields 10^8
		// iterations; at depth 1 the worst case is MaxStringLen, which is cheap.
		// Nested loops have no real use in a chat message.
		MaxLoopDepth:   1,
		MaxIterableLen: 1000,
		// Generous: a pasted document is a legitimate variable value.
		MaxStringLen: 10000,
	}
}

// Outcome records what happened to one message. Err is set whenever Rendered is
// false, and exists so callers can meter and log fallbacks: an attacker probing
// the restricted control-structure set produces nothing but fallbacks, and
// without metering that traffic is invisible.
type Outcome struct {
	Rendered bool
	Err      error
}

// Untrusted renders end-user text. It is safe for concurrent use.
type Untrusted struct {
	limits Limits
	env    *exec.Environment
	cfg    *config.Config
}

func NewUntrusted(limits Limits) *Untrusted {
	cfg := config.New()
	// Missing variables must fail rather than render empty, so the caller can
	// fall back to the original text instead of shipping a hollowed-out message.
	cfg.StrictUndefined = true

	return &Untrusted{
		limits: limits,
		cfg:    cfg,
		env: &exec.Environment{
			// An empty global context drops range, lipsum and cycler — none of
			// which end-user text has any use for, and range is an unbounded
			// iteration source.
			Context: exec.NewContext(map[string]any{}),
			// Restricted, not builtins.Filters: the sizing filters allocate
			// proportionally to a caller-supplied integer, materialising the
			// value in memory before the budgeted writer ever sees a byte.
			// {{ s | center(200000000) }} is 27 bytes and allocates 1091 MB.
			Filters:           UntrustedFilters(),
			Tests:             builtins.Tests,
			ControlStructures: UntrustedControlStructures(),
			Methods:           builtins.Methods,
		},
	}
}

// Render renders src against the allowlisted subset of vars.
//
// It never returns an error: any parse or execution failure, cap trip, or guard
// rejection returns src unchanged with Outcome.Rendered false. Partial output
// from a render that failed mid-way is discarded.
func (u *Untrusted) Render(src string, vars map[string]any, allowed []string, budget *Budget) (string, Outcome) {
	if len(src) > u.limits.MaxTemplateBytes {
		return src, Outcome{Err: ErrTemplateTooLarge}
	}

	// MUST run before exec.NewTemplate. gonja's parser is not recursion-safe and
	// overflows the stack fatally on deeply nested expressions — a failure no
	// recover() catches and no post-parse guard can reach, because the process
	// is gone before the parse tree exists.
	if err := GuardExpressionDepth(src, u.limits.MaxExpressionDepth); err != nil {
		return src, Outcome{Err: err}
	}

	scoped := Allowlist(vars, allowed)
	if err := CheckIterables(scoped, u.limits.MaxIterableLen, u.limits.MaxStringLen); err != nil {
		return src, Outcome{Err: err}
	}

	ldr, err := loaders.NewMemoryLoader(map[string]string{untrustedKey: src})
	if err != nil {
		return src, Outcome{Err: err}
	}

	tpl, err := exec.NewTemplate(untrustedKey, u.cfg, ldr, u.env)
	if err != nil {
		return src, Outcome{Err: err}
	}

	if err := GuardLoops(tpl.Root(), u.limits.MaxLoopDepth); err != nil {
		return src, Outcome{Err: err}
	}

	var buf bytes.Buffer
	if err := tpl.Execute(budget.Writer(&buf), exec.NewContext(scoped)); err != nil {
		// buf may hold partial output; discard it.
		return src, Outcome{Err: err}
	}
	return buf.String(), Outcome{Rendered: true}
}
```

Add to `engine/vars.go`:

```go
// ErrTemplateTooLarge reports a message longer than the permitted template size.
var ErrTemplateTooLarge = errors.New("template exceeds maximum size")
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./engine/ -v`
Expected: PASS, all tasks' tests. `TestUntrustedNeutralisesKnownAttacks` passing is the acceptance signal for the whole security design.

- [ ] **Step 5: Commit**

```bash
git add engine/untrusted.go engine/vars.go engine/untrusted_test.go
git commit -m "feat(engine): untrusted renderer with verbatim fallback"
```

---

### Task 7: Strict-undefined error surfacing

The authored path returns 400 listing the missing variable names, so gonja's error text has to be turned into a typed error carrying those names.

**Files:**
- Create: `engine/strict.go`
- Test: `engine/strict_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces:
  - `type MissingVariablesError struct{ Names []string }`
  - `func (e *MissingVariablesError) Error() string`
  - `func AsMissingVariables(err error) (*MissingVariablesError, bool)`

- [ ] **Step 1: Write the failing test**

Create `engine/strict_test.go`:

```go
package engine_test

import (
	"errors"
	"testing"

	"github.com/arbi-ai/bifrost-prompt-templates/engine"
	"github.com/stretchr/testify/require"
)

func TestAsMissingVariablesExtractsName(t *testing.T) {
	err := errors.New(`unable to execute template: Unable to render expression at line 1: nope: Unable to evaluate name "nope"`)
	mv, ok := engine.AsMissingVariables(err)
	require.True(t, ok)
	require.Equal(t, []string{"nope"}, mv.Names)
}

func TestAsMissingVariablesExtractsAttributePath(t *testing.T) {
	err := errors.New(`unable to execute template: Unable to render expression at line 1: user.city: Unable to evaluate user.city: attribute 'city' not found`)
	mv, ok := engine.AsMissingVariables(err)
	require.True(t, ok)
	require.Equal(t, []string{"user.city"}, mv.Names)
}

func TestAsMissingVariablesIgnoresUnrelatedErrors(t *testing.T) {
	_, ok := engine.AsMissingVariables(errors.New("connection refused"))
	require.False(t, ok)
}

func TestMissingVariablesErrorMessageListsNames(t *testing.T) {
	e := &engine.MissingVariablesError{Names: []string{"company", "tier"}}
	require.Contains(t, e.Error(), "company")
	require.Contains(t, e.Error(), "tier")
}

func TestAsMissingVariablesUnwrapsTypedError(t *testing.T) {
	inner := &engine.MissingVariablesError{Names: []string{"a"}}
	mv, ok := engine.AsMissingVariables(errors.Join(errors.New("ctx"), inner))
	require.True(t, ok)
	require.Equal(t, []string{"a"}, mv.Names)
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./engine/ -run "TestAsMissingVariables|TestMissingVariables" -v`
Expected: FAIL — `undefined: engine.AsMissingVariables`.

- [ ] **Step 3: Implement**

Create `engine/strict.go`:

```go
package engine

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// MissingVariablesError reports template variables that had no supplied value
// under StrictUndefined. The authored path turns this into a 400 naming them.
type MissingVariablesError struct {
	Names []string
}

func (e *MissingVariablesError) Error() string {
	return fmt.Sprintf("missing prompt variables: %s", strings.Join(e.Names, ", "))
}

// gonja reports undefined names and attributes in two distinct shapes. Matching
// on message text is unavoidable: the evaluator returns plain errors with no
// typed variant to inspect. Both patterns are covered by tests so a gonja
// upgrade that changes the wording fails loudly rather than silently.
var (
	undefinedNamePattern = regexp.MustCompile(`Unable to evaluate name "([^"]+)"`)
	undefinedAttrPattern = regexp.MustCompile(`Unable to evaluate ([\w.]+): attribute '[^']+' not found`)
)

// AsMissingVariables reports whether err describes undefined template variables,
// returning them by name.
func AsMissingVariables(err error) (*MissingVariablesError, bool) {
	if err == nil {
		return nil, false
	}

	var typed *MissingVariablesError
	if errors.As(err, &typed) {
		return typed, true
	}

	msg := err.Error()
	seen := make(map[string]struct{})
	var names []string
	for _, pattern := range []*regexp.Regexp{undefinedNamePattern, undefinedAttrPattern} {
		for _, match := range pattern.FindAllStringSubmatch(msg, -1) {
			name := match[1]
			if _, dup := seen[name]; dup {
				continue
			}
			seen[name] = struct{}{}
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return nil, false
	}
	return &MissingVariablesError{Names: names}, true
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./engine/ -run "TestAsMissingVariables|TestMissingVariables" -v`
Expected: PASS (5 tests).

- [ ] **Step 5: Commit**

```bash
git add engine/strict.go engine/strict_test.go
git commit -m "feat(engine): typed missing-variable errors from strict mode"
```

---

### Task 8: Variable merge and precedence

**Files:**
- Create: `render/merge.go`
- Test: `render/merge_test.go`

**Interfaces:**
- Consumes: nothing from `engine`.
- Produces:
  - `func Merge(versionDefaults, header, body map[string]any) map[string]any`
  - `func DeclaredNames(versionDefaults map[string]any) []string`

- [ ] **Step 1: Write the failing test**

Create `render/merge_test.go`:

```go
package render_test

import (
	"testing"

	"github.com/arbi-ai/bifrost-prompt-templates/render"
	"github.com/stretchr/testify/require"
)

func TestMergePrefersBodyOverHeaderOverDefaults(t *testing.T) {
	got := render.Merge(
		map[string]any{"tier": "free", "region": "us"},
		map[string]any{"tier": "plus"},
		map[string]any{"tier": "pro"},
	)
	require.Equal(t, "pro", got["tier"])
	require.Equal(t, "us", got["region"])
}

func TestMergeHeaderWinsWhenBodyAbsent(t *testing.T) {
	got := render.Merge(
		map[string]any{"tier": "free"},
		map[string]any{"tier": "plus"},
		nil,
	)
	require.Equal(t, "plus", got["tier"])
}

// The prompt_versions table stores declared NAMES with empty values. Treating
// "" as a supplied value would put every declared name in the map, so
// StrictUndefined would never fire and the 400-with-missing-names behaviour —
// the entire point of the strict/lenient split — would silently degrade into
// rendering empty strings.
func TestMergeTreatsEmptyDefaultAsAbsent(t *testing.T) {
	got := render.Merge(map[string]any{"company": ""}, nil, nil)
	require.NotContains(t, got, "company")
}

func TestMergeKeepsNonEmptyDefault(t *testing.T) {
	got := render.Merge(map[string]any{"company": "Arbi"}, nil, nil)
	require.Equal(t, "Arbi", got["company"])
}

// Empty is only special for DEFAULTS. A caller may legitimately send "".
func TestMergeKeepsExplicitEmptyFromBody(t *testing.T) {
	got := render.Merge(map[string]any{"note": "default"}, nil, map[string]any{"note": ""})
	require.Contains(t, got, "note")
	require.Equal(t, "", got["note"])
}

func TestMergeKeepsNonStringDefaults(t *testing.T) {
	got := render.Merge(map[string]any{"orders": []any{"a"}}, nil, nil)
	require.Equal(t, []any{"a"}, got["orders"])
}

func TestDeclaredNamesListsAllKeysIncludingEmpty(t *testing.T) {
	got := render.DeclaredNames(map[string]any{"company": "", "tier": "pro"})
	require.ElementsMatch(t, []string{"company", "tier"}, got)
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./render/ -v`
Expected: FAIL — package `render` does not exist.

- [ ] **Step 3: Implement**

Create `render/merge.go`:

```go
// Package render assembles variable sets and applies them to prompt messages.
package render

// Merge combines the three variable sources by precedence, lowest first:
// version-declared defaults, the x-bf-prompt-variables header, then the request
// body. The merge is shallow, by top-level key.
//
// An empty-string DEFAULT is treated as absent. The prompt_versions table stores
// declared variable names with empty values, so admitting them would satisfy
// StrictUndefined for every declared name and defeat the missing-variable 400.
// This applies only to defaults: an explicit "" from the header or body is a
// real value and is kept.
func Merge(versionDefaults, header, body map[string]any) map[string]any {
	merged := make(map[string]any, len(versionDefaults)+len(header)+len(body))

	for key, value := range versionDefaults {
		if s, isString := value.(string); isString && s == "" {
			continue
		}
		merged[key] = value
	}
	for key, value := range header {
		merged[key] = value
	}
	for key, value := range body {
		merged[key] = value
	}
	return merged
}

// DeclaredNames returns every variable name a prompt version declares, including
// those whose default is empty. This is the allowlist handed to the untrusted
// renderer, so a client message can reference declared variables but nothing else.
func DeclaredNames(versionDefaults map[string]any) []string {
	names := make([]string, 0, len(versionDefaults))
	for key := range versionDefaults {
		names = append(names, key)
	}
	return names
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./render/ -v`
Expected: PASS (7 tests).

- [ ] **Step 5: Commit**

```bash
git add render/merge.go render/merge_test.go
git commit -m "feat(render): variable precedence with empty defaults treated as absent"
```

---

### Task 9: CI and the full-suite gate

**Files:**
- Create: `.github/workflows/ci.yml`
- Create: `Makefile`

**Interfaces:**
- Consumes: every earlier task.
- Produces: `make test`, `make lint`.

- [ ] **Step 1: Add the Makefile**

```makefile
.PHONY: test lint race

test:
	go test ./... -v

race:
	go test ./... -race

lint:
	go vet ./...
	@test -z "$$(gofmt -l .)" || (gofmt -l .; exit 1)
```

- [ ] **Step 2: Run the whole suite**

Run: `make test`
Expected: PASS across `engine` and `render`.

- [ ] **Step 3: Run with the race detector**

Run: `make race`
Expected: PASS. `Untrusted` and `Budget` are both documented as concurrency-safe and this is the check.

- [ ] **Step 4: Add CI**

Create `.github/workflows/ci.yml`:

```yaml
name: ci
on:
  push:
    branches: [main]
  pull_request:

jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: '1.26.6'
      - run: make lint
      - run: make test
      - run: make race
```

- [ ] **Step 5: Commit**

```bash
git add Makefile .github/workflows/ci.yml
git commit -m "ci: test, race and lint gates"
```

---

## Out of scope for this plan

These are the remaining spec milestones. Each gets its own plan, and each depends on this one:

- **Plan 2 — Authored renderer and partials.** Depth-guarded replacements for `include`/`extends`/`import`, the partial registry, and the compiled-template cache keyed on resolved version plus partial-set fingerprint.
- **Plan 3 — Plugin and fork wiring.** `store/` interfaces, the `Plugin` type with its hooks, message walking for chat and Responses, the `ExtraParams` strip, the `prompts` coexistence boot check, and the `plugins.go` case.
- **Plan 4 — `/render` endpoint and partial CRUD.** Handler on the inference chain, the `prompt_partials` table and migration, CRUD on the API chain.
- **Plan 5 — UI and `.so` distribution.** Partials editor, render preview, `cmd/plugin` build, degraded-mode docs.

## Self-Review

**Spec coverage for this plan's scope.** Untrusted environment → Task 2. Output cap → Task 3. Iteration bounding → Tasks 4 and 5. Verbatim fallback and partial-output discard → Task 6. Attack table → Task 6's `TestUntrustedNeutralisesKnownAttacks` plus Task 2's parse rejections. Strict-mode 400 names → Task 7. Precedence and the empty-default trap → Task 8. `gonja.FromString` ban → Task 1.

**Also covered after the second review:** parser recursion → Task 5b. Sizing-filter allocation → Task 5c. The fail-closed AST walk and the `if`/`elif`/`else` branch cases → Task 4. Strings counting as iterables, and non-JSON slice types → Task 5.

**Two spec items deliberately deferred with a note rather than silently dropped:** `max_total_iterations` as a runtime counter is replaced by the depth guard plus length caps, because `forParser` is unexported and cannot be wrapped without vendoring. `render_timeout` belongs to the plugin layer, which owns the goroutine, and lands in Plan 3.

**The iteration bound, stated precisely.** `MaxLoopDepth` is 1, so the worst case is a single loop over the longest permitted iterable: `MaxStringLen` (10 000) for a string, `MaxIterableLen` (1 000) for a collection. Depth, not length, is what makes this cheap — at depth 2 a long string variable yields 10⁸ iterations. Raising `MaxLoopDepth` therefore requires re-deriving the bound, not just editing a constant.

**Not verified, and flagged for the implementer:** Task 5c assumes `exec.FilterSet` exposes enumeration comparable to `ControlStructureSet.Get`. `Get` is confirmed to exist on `ControlStructureSet`; the filter-set equivalent was not confirmed. The task carries an explicit fallback and a prohibition on reverting to the unmodified builtin set.

**Type consistency.** `Limits`, `Budget`, and `Outcome` are defined in Task 3/6 and used consistently; `Allowlist` and `CheckIterables` signatures match their Task 5 definitions and their Task 6 call sites; `DefaultLimits()` field names match the struct.
