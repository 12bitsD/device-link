# Device Link — Agent Engineering Contract

## Scope & Boundaries

- Single-user, single-trust-domain remote device fabric for humans and agents.
- One static Go binary `dl` (macOS / Linux / Windows, `amd64` / `arm64`, `CGO_ENABLED=0`).
- Roles are strictly separated: **Node** is controlled only; **Controller** holds the controller token to list/manage devices and issue commands. Node credentials must fail closed (401/403) on controller endpoints.
- Out of scope for v1: interactive PTY, screenshot, P2P NAT hole punching, multi-tenant RBAC, local audit ledger.

## Authoritative Docs

1. `docs/spec.md` — v1 behavior contract, CLI surface, wire expectations, and rollout stages.
2. `docs/steps/Sx.y-*.md` — per-slice Step Card (written before any test or implementation code).
3. Bits Coding Style (`BCS001`–`BCS015`) & Go Profile (`GO001`–`GO010`):
   - Correctness > clarity > modifiability > consistency > brevity.
   - Guard clauses keep the happy path left-aligned; no `else` after `return`/`break`/`continue`.
   - Explicit error paths (`%w`, `errors.Is/As`); no `panic`/`log.Fatal`/`os.Exit` outside `cmd/dl` and tests; cleanup errors explicitly discarded (`_ = c.Close()`).
   - `context.Context` is always the first parameter and never stored in structs or globals.
   - Every goroutine has an explicit owner, cancellation path, and join/teardown path.
   - Nesting depth <= 3, cyclomatic complexity <= 10, params <= 4 (warning above 4), no naked `bool` params, no parameter reassignment or parameter shadowing, no mutable globals, no `init()`.
   - Zero values, enums, and small structs express constraints instead of `map[string]any` or boolean state soup.
   - Interfaces are consumer-defined (1–3 methods); no single-implementation interfaces without a real process/test seam, no pass-through wrappers, no `util/common/base/helpers` packages.
   - `GO010`: composition must not hide behavior via deep/anonymous struct embedding.
   - Abstract only on the third stable repetition; prefer code that can be deleted over code that is merely shorter.

## Step Card (Gate 0 — Required Before Coding)

Each slice (~150–400 net LOC) starts with `docs/steps/Sx.y-<slug>.md` (30–50 lines) containing:
1. **Value & acceptance scenarios**: exact `./dl` invocations, normal/failure/disconnect outputs.
2. **In scope / Out of scope (forbidden)**: explicitly ban future-slice flags, fields, and hooks.
3. **CLI / Wire delta**: subcommands, HTTP routes, JSON/WS message fields.
4. **Simplicity budget**: default 0 new packages, 0 new deps, 0 new interfaces; justify any exception and list reuse/deletion candidates.
5. **Data-structure choice**: how types/zero values/tables eliminate special-case branches.

## Development Loop (Mandatory — 6 Gates per Slice)

1. **Gate 1 — Red E2E (clean-context test subagent)**: Reads only `docs/spec.md`, the Step Card, and `test/e2e/harness_test.go` (never `internal/`). Writes black-box tests against the real compiled `./dl` binary and runs them to prove they fail for the missing behavior, not a test bug.
2. **Gate 2 — Minimal implementation**: Implement only what makes the red tests green. No placeholder fields, inert flags, pass-through wrappers, or speculative abstractions. Small adjudication fixes are done directly in the main turn.
3. **Gate 3 — Machine gates**: `./scripts/verify.sh` and `./scripts/bench.sh` must pass; no unexplained baseline regression.
4. **Gate 4 — Blind 3-agent review (clean contexts, no cross-visibility)**: Reviewers see only the diff, Step Card, `docs/spec.md`, and `AGENTS.md`:
   - **A (Simplicity & data structures)**: what lines can be deleted? premature interfaces, wrappers, duplicated logic, branchy code replaceable by data.
   - **B (Concurrency, lifecycle & errors)**: goroutine owner/cancel/join, lock scope/copy safety, WS/HTTP shutdown leaks, error wrapping.
   - **C (Contract fidelity & dead code)**: every flag/field/branch/status code justified by spec; unused config, 500 masked as 404, missing edge assertions.
5. **Gate 5 — Main-turn adjudication (Delete-First)**: Triage into `Must Fix`, `Simplify` (delete/de-duplicate), or `Reject` (including reviewer over-engineering). Prefer fixes that reduce net LOC. Re-run Gate 3 before committing.

## Stage-Level Optimization & Iteration Loop

At the end of each Stage:
1. Cross-review and deduplicate findings across slices.
2. **Deletion Pass**: a dedicated delete/consolidate pass; collapse stable third-occurrence duplication only if net LOC drops; remove transitional scaffolding.
3. **Measured optimization**: optimize only hotspots shown by `bench.sh` (binary size, allocs, WS latency, SQLite writes); keep before/after numbers.
4. **Checker feedback**: convert repeatable review anti-patterns into `internal/stylecheck` AST rules with `testdata` fixtures.
5. Lock the baseline and commit the Stage milestone.

## Exemptions

- `bits:allow(RULE): <reason>` on the function doc or immediately preceding line only, for real external contracts. No file/package blanket suppressions.
- `bits:explain(BCS003): <responsibility>; <why cohesive>; <split risk>` is a last resort for cohesive functions, never a substitute for a clean refactor.

## Verification Commands

```sh
./scripts/verify.sh   # gofmt + go vet + staticcheck + deadcode + Bits AST style + bits report + go test -race + 6-target cross-compile
./scripts/bench.sh    # binary size, LOC, ns/op + B/op + allocs/op benchmarks
```
