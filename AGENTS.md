# Device Link — Agent Engineering Contract

## Scope & Boundaries

- Single-user, single-trust-domain remote device fabric for humans and agents.
- One static Go binary `dl` (macOS / Linux / Windows, `amd64` / `arm64`, `CGO_ENABLED=0`).
- Roles are strictly separated: **Node** is controlled only; **Controller** holds the controller token to list/manage devices and issue commands. Node credentials must fail closed (401/403) on controller endpoints.
- Out of scope for v1: interactive PTY, screenshot, P2P NAT hole punching, multi-tenant RBAC, local audit ledger.

## Authoritative Docs

1. `docs/spec.md` — v1 behavior contract, CLI surface, wire expectations, and rollout stages.
2. Bits Coding Style (`BCS001`–`BCS015`) & Go Profile (`GO001`–`GO009`):
   - Correctness > clarity > modifiability > consistency > brevity.
   - Guard clauses keep the happy path left-aligned; no `else` after `return`.
   - Explicit error paths (`%w`, `errors.Is/As`); cleanup errors explicitly discarded (`_ = c.Close()`).
   - `context.Context` is always the first parameter and never stored in structs or globals.
   - Every goroutine has an explicit owner, cancellation path, and join/teardown path.
   - Nesting depth <= 3, cyclomatic complexity <= 10, params <= 4, no naked `bool` params, no mutable globals, no variable shadowing.
   - No speculative interfaces, pass-through wrappers, or `util/common` packages.

## Development Loop (Mandatory)

1. Write failing black-box E2E tests against the real compiled `./dl` binary first (separate context).
2. Implement until `./scripts/verify.sh` is green. Never weaken or delete acceptance assertions to pass.
3. Run clean-context 3-agent taste review (simplicity/data structures, concurrency/resources, single source of truth/readability) + cross-review; adjudicate findings in the main turn.
4. Run `./scripts/verify.sh` and `./scripts/bench.sh`; keep only changes that pass and do not regress the baseline.

## Verification Commands

```sh
./scripts/verify.sh   # gofmt + go vet + staticcheck + AST style + go test -race + 6-target cross-compile
./scripts/bench.sh    # binary size, LOC, complexity, latency/throughput/memory baselines
```
