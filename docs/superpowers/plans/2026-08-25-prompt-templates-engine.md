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

**Divergence from the spec, applied here deliberately:** the spec's untrusted control-structure set is `if`/`for`/`raw`/`with`/assignment-`set`. This plan drops `set` entirely. Both `set` forms share one `setParser`, and the block form (`{% set x %}…{% endset %}`) is one of the four output-cap bypasses; the marker distinguishing them (`cs.body`) is unexported, so they cannot be separated from outside the package. Client messages have no need for `{% set %}`. The spec needs a one-line correction to match.

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
		"if":   `{% if tier == 'pro' %}PRO{% else %}FREE{% endif %}`,
		"for":  `{% for o in orders %}[{{ o.id }}]{% endfor %}`,
		"raw":  `{% raw %}{{ literal }}{% endraw %}`,
		"with": `{% with y = 1 %}{{ y }}{% endwith %}`,
		"var":  `Hi {{ name }}`,
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
//
// set is excluded in both its forms: they share one parser and the block-form
// marker is unexported, so the safe assignment form cannot be admitted alone.
var UntrustedAllowed = []string{"if", "for", "raw", "with"}

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

func TestGuardWalksIntoIfBranches(t *testing.T) {
	src := `{% if c %}{% for a in xs %}{% for b in xs %}{% for d in xs %}{% endfor %}{% endfor %}{% endfor %}{% endif %}`
	tpl := parseTreeForGuard(t, src)
	require.ErrorIs(t, engine.GuardLoops(tpl.Root(), 2), engine.ErrLoopTooDeep)
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
	"reflect"

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

// GuardLoops walks a parsed template and rejects loop nesting deeper than
// maxDepth, and any recursive loop.
//
// The walk uses reflection because gonja's node types expose their children
// through differently-named exported fields (BodyWrapper, EmptyWrapper, Wrapper)
// with no common interface. Reflection over exported fields keeps the guard
// correct as node types are added, at the cost of being slower than a type
// switch — acceptable, since it runs once per parse, not per iteration.
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

func walkNode(n nodes.Node, depth, maxDepth int) error {
	if n == nil {
		return nil
	}
	if loop, ok := n.(*controlStructures.ForControlStructure); ok {
		if loop.Recursive {
			return ErrRecursiveLoop
		}
		depth++
		if depth > maxDepth {
			return ErrLoopTooDeep
		}
	}
	return walkChildren(reflect.ValueOf(n), depth, maxDepth)
}

// walkChildren descends through any exported field that is a *nodes.Wrapper or
// a []nodes.Node, which is how every gonja control structure holds its body.
func walkChildren(v reflect.Value, depth, maxDepth int) error {
	for v.Kind() == reflect.Ptr || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return nil
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return nil
	}
	for i := 0; i < v.NumField(); i++ {
		if !v.Type().Field(i).IsExported() {
			continue
		}
		field := v.Field(i)
		switch child := field.Interface().(type) {
		case *nodes.Wrapper:
			if child == nil {
				continue
			}
			if err := walkNodes(child.Nodes, depth, maxDepth); err != nil {
				return err
			}
		case []nodes.Node:
			if err := walkNodes(child, depth, maxDepth); err != nil {
				return err
			}
		case nodes.Node:
			if err := walkNode(child, depth, maxDepth); err != nil {
				return err
			}
		}
	}
	return nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./engine/ -run TestGuard -v`
Expected: PASS (4 tests). If `TestGuardWalksIntoIfBranches` fails, the `if` node holds its branches in a field shape the switch does not cover — print `reflect.TypeOf(n)` for the failing node and add that shape to `walkChildren`.

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

// A long string is not an iteration hazard and must not be rejected.
func TestCheckIterablesIgnoresStringLength(t *testing.T) {
	vars := map[string]any{"doc": string(make([]byte, 100_000))}
	require.NoError(t, engine.CheckIterables(vars, 100))
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

// CheckIterables reports an error if any collection reachable from vars is
// longer than maxLen. Strings are exempt: their length costs bytes, which the
// output budget already governs, not iterations.
func CheckIterables(vars map[string]any, maxLen int) error {
	for _, v := range vars {
		if err := checkValue(v, maxLen); err != nil {
			return err
		}
	}
	return nil
}

func checkValue(v any, maxLen int) error {
	switch typed := v.(type) {
	case []any:
		if len(typed) > maxLen {
			return ErrIterableTooLarge
		}
		for _, item := range typed {
			if err := checkValue(item, maxLen); err != nil {
				return err
			}
		}
	case map[string]any:
		if len(typed) > maxLen {
			return ErrIterableTooLarge
		}
		for _, item := range typed {
			if err := checkValue(item, maxLen); err != nil {
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
	const src = "PREFIX-{{ missing }}"
	out, oc := renderUntrusted(t, src, map[string]any{}, []string{})
	require.False(t, oc.Rendered)
	require.Equal(t, src, out)
	require.NotContains(t, out, "PREFIX-PREFIX")
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
	MaxOutputBytes   int64
	MaxTemplateBytes int
	MaxLoopDepth     int
	MaxIterableLen   int
}

func DefaultLimits() Limits {
	return Limits{
		MaxOutputBytes:   1 << 20, // 1 MiB, per request
		MaxTemplateBytes: 256 << 10,
		MaxLoopDepth:     2,
		MaxIterableLen:   1000,
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
			Context:           exec.NewContext(map[string]any{}),
			Filters:           builtins.Filters,
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

	scoped := Allowlist(vars, allowed)
	if err := CheckIterables(scoped, u.limits.MaxIterableLen); err != nil {
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
Expected: FAIL — `undefined: engine.AsMissingVariablesError`.

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
	gofmt -l . | tee /dev/stderr | (! read)
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

**Two spec items deliberately deferred with a note rather than silently dropped:** `max_total_iterations` as a runtime counter is replaced by the depth guard plus iterable cap, because `forParser` is unexported and cannot be wrapped without vendoring; the pair bounds iterations at `MaxIterableLen^MaxLoopDepth` (10⁶ at the defaults). `render_timeout` belongs to the plugin layer, which owns the goroutine, and lands in Plan 3.

**Type consistency.** `Limits`, `Budget`, and `Outcome` are defined in Task 3/6 and used consistently; `Allowlist` and `CheckIterables` signatures match their Task 5 definitions and their Task 6 call sites; `DefaultLimits()` field names match the struct.
