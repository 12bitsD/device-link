# Device Link (`dl`) v1 Specification

Device Link is an ultra-lightweight, single-tenant protocol and single-binary Go tool (`dl`) for cross-platform (macOS / Windows / Linux / K8s Pod) remote command execution (`exec`) and streaming file transfer (`<= 500MB`). Humans and agents use the exact same CLI/protocol.

## 1. Roles & Trust Model

- **Hub**: Self-hosted lightweight public HTTPS server (`dl hub`) that solves double-NAT (home Wi-Fi <-> mobile hotspot / private K8s Pod without public IP). All nodes dial outbound to the Hub.
- **Node (controlled device)**: Runs `dl join`. Holds only its own long-term `device_token`. Can maintain its connection and execute jobs locally; **cannot** enumerate or control other devices (controller APIs return 401/403 fail-closed).
- **Controller**: Any machine holding the Hub's controller token (`--token` flag or `DL_TOKEN` env / `--state-dir` config). Runs `invite`, `ls`, `ttl`, `rm`, `exec`, `job`, `cp`, `transfer`.

## 2. Onboarding, TTL & Namespace

- `dl hub --addr :8443 --state-dir <dir>` initializes the Hub state (pure-Go SQLite, single file) and prints/stores the initial controller token.
- `dl invite --name <name> [--ttl 24h]` (controller) generates a one-time short-lived invite code and prints a single paste-ready command:
  `dl join --hub <url> --code <code> [--service]`
- `dl join` exchanges the one-time invite code for a persistent `device_token` stored in the node's `--state-dir`, then opens one persistent WebSocket control connection to the Hub.
- Each device record has `expires_at` (`null` by default = never expires). Controller can change it via `dl ttl <name> <duration|never>` or revoke & kick via `dl rm <name>`.
- Hub maintains a single namespace mapping `name -> live connection`:
  - Name collision from a *different* device auto-appends an incrementing suffix: `mac` -> `mac-2` -> `mac-3`.
  - Same device reconnecting (network flap / hotspot switch) with its `device_token` reclaims its original assigned name.
- `dl ls [--json]` returns minimal metadata only: `name`, `status` (`alive` | `missed`), `os`, `arch`, `shell`, `expires_at`, `last_seen`. Everything else (disk, cwd, hardware) is queried on demand via `exec`.
- Heartbeats detect dead connections (e.g. ~3s missed heartbeat marks `missed` while keeping the registry entry and `last_seen`). Nodes auto-reconnect with backoff.

## 3. Remote Shell (`Exec` only, Stage 2)

- Non-interactive `exec` with the target user's normal login `PATH`; `stdin` closed by default.
- Timeout or `kill` terminates the whole process tree (Unix process group `pgid` / Windows Job Object).
- Every run gets a `job_id`; output is spooled to a temp log on the node.
  - Finishes within `--wait` window: synchronous return of `stdout` / `stderr` / `exit_code`.
  - Exceeds `--wait` or `--detach`: returns `job_id`.
- If the node loses network mid-job, the local process keeps running and caching output; after reconnect, the controller resumes reading via `dl job logs <job_id> --offset <bytes>`.
- `dl exec --wait-online` optionally waits briefly for an offline device to reconnect.

## 4. File Transfer (`<= 500MB`, Stage 3)

- Four unambiguous elements: `from + sources[] -> to + target_dir`.
  - CLI: `dl cp [from:]<src>... [to:]<target_dir>` (omitted prefix means controller-local `local`).
  - Supports `A->B`, `local->B`, `A->local`; single file, directory, or multiple paths copied by base name into `target_dir`; `--exclude` patterns.
- Pre-flight source size scan rejects `> 500MB` before streaming and lists the largest offending files.
- Same-name target files compared by SHA-256: identical skipped, different overwritten by default while preserving `+x` permission bits.
- Transferred via independent HTTP `tar` streams piped in-memory through the Hub (zero Hub disk spool; large transfers never block the WebSocket control channel).
- Slow transfers return a `transfer_id` for `dl transfer status` / `dl transfer cancel`.

## 5. Architecture & CLI Surface (13 Subcommands)

- **Stack**: Go stdlib + pure-Go WebSocket (`github.com/coder/websocket`) + pure-Go SQLite (`modernc.org/sqlite`, `CGO_ENABLED=0`).
- **Transport**: Single HTTPS port (self-signed TLS supported for v1, fingerprint/CA pinned or configurable) physically splits WebSocket control stream vs. HTTP `tar` byte streams.
- **Commands**:
  1. Lifecycle: `dl hub`, `dl invite`, `dl join [--service]`
  2. Registry: `dl ls [--json]`, `dl ttl`, `dl rm`
  3. Exec: `dl exec`, `dl job status`, `dl job logs`, `dl job kill`
  4. Transfer: `dl cp`, `dl transfer status`, `dl transfer cancel`

## 6. Rollout Stages

- **Stage 1**: Hub registry (SQLite) + one-time invite / device token auth + WebSocket long connection + heartbeat (`alive`/`missed`) + Namespace auto-numbering + `invite` / `join` / `ls` / `ttl` / `rm` + role isolation.
- **Stage 2**: Remote `exec` (`--wait`, `--detach`, `--wait-online`) + offline continuation & log resume (`--offset`) + process-group kill.
- **Stage 3**: `<= 500MB` pre-flight + SHA-256 skip + independent HTTP `tar` in-memory Hub relay + transfer status/cancel.
- **Stage 4**: OS background services (`launchd` / `systemd` / Windows Service) + GitHub Actions 3-OS release pipeline.

## 7. Black-Box Verification Contract

- Tests exercise the real compiled `./dl` binary in isolated multi-process setups (`--state-dir` per process, ephemeral localhost ports).
- Local TCP fault/throttle proxy simulates network drops (3s drop -> `missed`, offline job continuation & resume) and throttled transfers concurrent with `exec`.
- Each test asserts no goroutine or temp-file leaks on teardown; GitHub Actions runs `ubuntu-latest`, `macos-latest`, `windows-latest`.
