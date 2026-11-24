# Device Link — Agent Engineering & Verification Contract

## 1. Scope & Hard Boundaries

- Single-user, single-trust-domain remote device tool and protocol for both humans and AI agents.
- Single static Go binary `dl` (`darwin` / `linux` / `windows` × `amd64` / `arm64`, `CGO_ENABLED=0`).
- Strict role separation: **Node** (`device_token`) is controlled only and must fail closed (`401`/`403`) on all controller endpoints; **Controller** (`controller_token`) lists/manages devices and issues `exec` / `cp`.
- Out of scope for v1: interactive PTY, screenshot/GUI, public P2P NAT hole punching, multi-tenant RBAC, local audit ledger, inert flags/interfaces reserved for future stages.

---

## 2. Coding Standards: Bits Coding Style (`BCS001`–`BCS015` & `GO001`–`GO010`)

Priority order: **Correctness > Clarity > Modifiability > Consistency > Brevity**.

- **Control flow (`BCS005`, `GO005`)**: Guard clauses keep the main path left-aligned; no `else` after `return`/`break`/`continue`. Nesting depth <= 3, cyclomatic complexity <= 10 (`BCS003`; >48 lines is a warning—split by cohesive phase, never mechanically by line count).
- **Signatures & state (`BCS004`, `BCS011`, `GO006`–`GO008`)**: Params <= 4 (warning above 4), no naked `bool` behavior switches, no parameter reassignment or shadowing, no mutable package globals, no `init()`. Express states with small structs/enums and usable zero values instead of `map[string]any` or boolean combinations.
- **Errors & resources (`BCS013`, `GO001`, `GO002`)**: Wrap errors with `%w`, check with `errors.Is/As`; no `panic`/`log.Fatal`/`os.Exit` outside `cmd/dl` and tests; release acquired resources via immediate `defer` in the same ownership scope; explicitly discard cleanup errors (`_ = c.Close()`).
- **Concurrency & lifecycle (`GO003`, `GO004`)**: `context.Context` is always the first parameter and never stored in structs. Every goroutine has an explicit owner, cancellation path, and wait/join teardown. Protect shared mutable state with explicit mutex/channel ownership; never copy mutexes or `WaitGroup`s.
- **Abstraction discipline (`GO009`, `GO010`)**: Consumer-defined interfaces (1–3 methods) only at real process/OS/test seams; no pass-through wrappers, no anonymous struct embedding that hides behavior, no `util/common/base/helpers` packages. Abstract only on the third stable repetition; prefer code that can be deleted.
- **Exemptions**: `bits:allow(RULE): <reason>` on the function doc or immediately preceding line only, strictly for external contracts. `bits:explain(BCS003): <responsibility>; <why cohesive>; <split risk>` only as a last resort.

---

## 3. Development Loop: 4 Stages × 4 Steps per Stage

Do **not** split stages into micro-slices or write per-slice Step Cards. Each Stage delivers one complete capability through **4 fixed Steps** (pause and report to the user if a Stage does not converge within 3 fix iterations):

```text
Step 1  Spec Check & Red Black-Box E2E  ──▶ Clean-context test agent writes E2E from docs/spec.md only; must fail red for missing behavior
  │
  ▼
Step 2  Complete Minimal Implementation ──▶ Implement full stage cleanly (never weaken Step 1 assertions); pass ./scripts/verify.sh
  │
  ▼
Step 3  Blind 3-Agent Review + Cross-Critique ──▶ 3 clean-context agents review independently, then cross-critique to drop over-engineering
  │
  ▼
Step 4  Main-Turn Adjudication, Delete & Lock ──▶ Red-first regression fix for real bugs → pure deletion pass → bench.sh check → Stage commit
```

### Step 1 — Spec Check & Independent Red E2E
1. Main turn verifies `docs/spec.md` faithfully captures the user's CLI UX, stdout/stderr text, exit codes, and failure modes for the Stage.
2. Spawn **1 clean-context test subagent** that reads **only** `docs/spec.md` and `test/e2e/harness_test.go` (forbidden from reading `cmd/` or `internal/`). It writes black-box tests invoking the compiled `./dl` binary across normal, network-fault, unauthorized, and boundary cases.
3. Run the new tests and confirm they **fail specifically because the target behavior is missing** (reject tests that fail due to bad flags/typos or pass vacuously).

### Step 2 — Complete Minimal Implementation & Machine Gates
1. Write the complete, minimal implementation for the Stage—directly using data structures and connection loops that sustain downstream stages (no throwaway scaffolding, no unused flags/types).
2. **Never delete or weaken Step 1 assertions** to turn tests green (if a test genuinely contradicts `docs/spec.md`, document the exact line mismatch in the main turn before adjusting).
3. Pass `./scripts/verify.sh` completely.

### Step 3 — Clean-Context 3-Agent Blind Review + Cross-Critique
1. **Round 1 (Independent Blind Review)**: Spawn **3 stateless read-only subagents** in parallel (given only the source/diff, `docs/spec.md`, `AGENTS.md`, and code antipattern rules—no implementation history):
   - **Reviewer A (Simplicity & Data Model)**: Identify lines/wrappers/types to delete and branches replaceable by cleaner data structures or zero values.
   - **Reviewer B (Concurrency, Lifecycle & Errors)**: Audit goroutine cancel/join ownership, lock scope, disconnect/timeout process & FD cleanup, and `%w` error propagation.
   - **Reviewer C (Contract Fidelity & Security)**: Check `docs/spec.md` omissions or scope creep, HTTP status masking (e.g., 500 turned into 401/404), authz fail-closed, and path/size boundary checks.
2. **Round 2 (Cross-Critique Filter)**: Feed combined findings back for one cross-critique round enforcing two hard rules:
   - Discard any finding lacking **exact `file:line` + concrete triggering input/interleaving (or violated rule ID)**;
   - Reject any suggestion that adds a new interface, package, or config knob without reducing net branching or fixing a reproducible failure.

### Step 4 — Main-Turn Adjudication, Deletion Pass & Baseline Lock
1. Main turn classifies surviving items into `Must-Fix Defect`, `Deletion/Simplification`, or `Rejected (violates scope or over-engineers)`.
2. Every `Must-Fix Defect` requires **writing a failing regression test first (red)**, then applying the minimal fix (green). Repeatable structural antipatterns are added to `internal/stylecheck` with positive/negative `testdata` fixtures.
3. Run one dedicated **delete-only pass** to collapse duplicate code and trim net LOC, then run `./scripts/verify.sh` and `./scripts/bench.sh`. Revert (`git checkout`) any "optimization" that fails to improve measured metrics or increases complexity.
4. Update `.bench/baseline.env` and commit the Stage milestone.

---

## 4. Six Verification Dimensions (Required Pass Criteria per Stage)

1. **Feature & CLI UX Fidelity**: Black-box `./dl` invocations verify real stdout/stderr text and exit codes (`invite` pasteable command, `join` post-connect status summary with `name`/`alive`/`os`/`arch`/`shell`/`expires_at`, and distinct errors for offline vs. unauthorized vs. timeout vs. command non-zero exit).
2. **Faults, Concurrency & Zero-Leak**: TCP fault/throttle proxy exercises disconnects (`missed` -> reconnect), slow streams concurrent with `exec`, grandchild process-tree kills, and `rm`/reconnect races; `harness_test.go` asserts **zero lingering child processes, zero leaked temp files, and zero stray Hub disk files** on cleanup; `./test/e2e` passes `go test -race -count=3` with zero flakes.
3. **Code Quality & Simplicity**: `gofmt` + `go vet` + `staticcheck` + `deadcode` (zero unreachable functions from `./cmd/dl`) + `internal/stylecheck` (Bits Go AST + fixtures) + `bits` CLI check on non-Go files; `bench.sh` tracks `src_loc`, binary size, and `allocs/op`.
4. **Security & Trust Boundaries**: Every controller route rejects `device_token` with `401`/`403`; revoked or expired nodes receiving `401`/`403` exit non-zero immediately (no infinite retry loop); Stage 3 rejects `>500MB` pre-stream and blocks `../`, absolute paths, and escaping symlinks before disk write; credential files enforce `0600`.
5. **Cross-Platform Reality**: `CGO_ENABLED=0` build across `darwin/linux/windows × amd64/arm64` + cross-compilation of `./test/e2e` for `linux` and `windows` + real Linux container (`colima`/`docker`) node-to-macOS-Hub E2E when container runtime is available + GitHub Actions `ubuntu-latest` / `macos-latest` / `windows-latest` matrix on push.
6. **Test & Review Meta-Quality**: New acceptance tests must fail red before implementation; bug fixes must fail red before the fix; review comments without concrete reproduction paths or net-simplification value are rejected.

---

## 5. Rollout Stages

1. **Stage 1 (Closeout & Alignment)**: Hub SQLite registry, one-time `invite`, `join` (with post-connect status output, async read/write WS loop ready for server-pushed frames, and `backoff` reset after healthy session), Namespace auto-numbering, `ls`, `ttl`, `rm`, harness zero-leak checks, and Linux container interop check.
2. **Stage 2 (Remote `exec` & Jobs)**: `exec` (`--cwd`, `--env`, `--wait`, `--timeout`, `--wait-online`, `--detach`) + `job status | logs | kill`, login `PATH` loading, closed `stdin`, offline continuation with `--offset` log resume, and Unix `pgid` / Windows Job Object tree termination.
3. **Stage 3 (`<= 500MB` Streaming `cp`)**: `cp` (`A->B`, `local->B`, `A->local` into `target_dir` + `--exclude`) + `transfer status | cancel`, 500MB pre-flight scan, SHA-256 skip, path-traversal defense, and independent HTTP `tar` zero-disk Hub relay without blocking concurrent `exec`.
4. **Stage 4 (OS Services & Release)**: `dl join --service` (`launchd` / `systemd` / Windows Service), GitHub Actions 3-OS E2E matrix + 6-target release artifacts, and one-line install/join script.

## 6. Verification Commands

```sh
./scripts/verify.sh   # gofmt + vet + staticcheck + deadcode + Bits AST + bits audit + unit (-race) + E2E (-race -count=3) + 6-target & test cross-compile
./scripts/bench.sh    # binary size, src/test LOC, baseline delta, and Go benchmarks (-benchmem)
```
