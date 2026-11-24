# Device Link (`dl`) v1 Specification

Device Link is an ultra-lightweight, single-tenant protocol and single-binary Go tool (`dl`) for cross-platform (macOS / Windows / Linux / K8s Pod) remote command execution (`exec`) and streaming file transfer (`<= 500MB`). Humans and agents use the exact same CLI/protocol.

## 1. Roles & Trust Model

- **Hub**: Self-hosted lightweight public HTTPS server (`dl hub`) that solves double-NAT (home Wi-Fi <-> mobile hotspot / private K8s Pod without public IP). All nodes dial outbound to the Hub.
- **Node (controlled device)**: Runs `dl join`. Holds only its own long-term `device_token`. Can maintain its connection and execute jobs locally; **cannot** enumerate or control other devices (controller APIs return 401/403 fail-closed). Revoked or expired device credentials (401/403) terminate `dl join` with a non-zero exit instead of retrying forever.
- **Controller**: Any machine holding the Hub's controller token (`--token` flag > `DL_TOKEN` env > `<state-dir>/controller_token`, where `--state-dir` defaults to `~/.device-link/hub`; `--ca` defaults to `<state-dir>/cert.pem` when omitted). Runs `invite`, `ls`, `ttl`, `rm`, `exec`, `job`, `cp`, `transfer`.

## 2. Onboarding, TTL & Namespace

- `dl hub --addr :8443 [--host <advertised-host-or-ip>] --state-dir <dir>` initializes the Hub state (pure-Go SQLite, single file) and prints/stores the initial controller token and bound `DL_ADDR`. The self-signed certificate always covers `localhost`/`127.0.0.1`/`::1`; `--host` adds a LAN/public DNS name or IP to its SANs.
- `dl invite --name <name> [--ttl 24h]` (controller) generates a one-time short-lived invite code and prints a single paste-ready command:
  `dl join --hub <url> --code <code> [--ca <path>]` (nodes may also pass `--service` in Stage 4)
- `dl join` exchanges the one-time invite code for a persistent `device_token` stored in the node's `--state-dir`, then opens one persistent WebSocket control connection to the Hub.
- Immediately upon establishing (or re-establishing) the WebSocket session, `dl join` prints a single status line to stdout:
  `dl connected: name=<name> status=alive platform=<os>/<arch> shell=<shell> expires_at=<never|RFC3339>`
  and resets its reconnect backoff to the initial interval (`250ms`) once the session is healthy. The WebSocket loop decouples reading incoming Hub frames from periodic heartbeat pings so Hub-pushed frames and `Close` frames are processed immediately without waiting for the next 1s ping tick.
- Each device record has `expires_at` (`null` by default = never expires). Controller can change it via `dl ttl <name> <duration|never>` or revoke & kick via `dl rm <name>`.
- Hub maintains a single namespace mapping `name -> live connection`:
  - Name collision from a *different* device auto-appends an incrementing suffix: `mac` -> `mac-2` -> `mac-3`.
  - Same device reconnecting (network flap / hotspot switch) with its `device_token` reclaims its original assigned name.
- `dl ls [--json]` returns minimal metadata only: `name`, `status` (`alive` | `missed`), `os`, `arch`, `shell`, `expires_at`, `last_seen` (human table columns: `NAME`, `STATUS`, `OS/ARCH`, `SHELL`, `EXPIRES`, `LAST SEEN`). Everything else (disk, cwd, hardware) is queried on demand via `exec`.
- Heartbeats detect dead connections (e.g. ~3s missed heartbeat marks `missed` while keeping the registry entry and `last_seen`). Nodes auto-reconnect with backoff.

## 3. Remote Shell (`Exec` only, Stage 2)

- **CLI Surface**:
  - `dl exec [controller-flags] [--cwd <dir>] [--env KEY=VALUE]... [--wait <duration>] [--timeout <duration>] [--wait-online <duration>] [--detach] <device> [--] <command> [args...]`
  - `dl job status [controller-flags] [--json] <job_id>`
  - `dl job logs [controller-flags] [--offset <bytes>] <job_id>` (flags such as `--offset` and `--json` are accepted either before or after `<job_id>`)
  - `dl job kill [controller-flags] <job_id>`
- **Execution Environment & Invocation**:
  - Non-interactive `exec` loads the target user's normal login `PATH` (unless explicitly overridden via `--env PATH=...`) and closes `stdin` (`EOF` immediately) by default.
  - `--cwd <dir>` sets the remote working directory (fails non-zero if missing/invalid); `--env KEY=VALUE` (repeatable) sets environment variables for the job.
  - When a single `<command>` argument is given, the node executes it via its shell (`<shell> -c <command>` on Unix; `cmd.exe /d /s /c` or `powershell.exe -NoProfile -NonInteractive -Command` on Windows). When multiple arguments (`<prog> <args...>`) are given, the node executes `<prog>` directly with `<args...>` resolved against the job `PATH`.
- **Synchronous vs. Detached Lifecycle**:
  - Every run gets a `job_id` and spools combined output in write order to `<node-state-dir>/jobs/<job_id>.log` on the node.
  - Finishes within `--wait` (default `10s`, when `--detach` is not set): writes remote `stdout` to controller `stdout` and remote `stderr` to controller `stderr` verbatim. Exits `0` on `completed`, exits with the remote command's non-zero `exit_code` on `failed`, prints `dl: job <job_id> timed out` to stderr and exits `124` on `timed_out`, and prints `dl: job <job_id> killed` to stderr and exits `137` on `killed`.
  - Exceeds `--wait` or `--detach` is set (or the node disconnects after starting the job): prints a single line `job_id=<job_id>` to `stdout` and exits `0` while the job continues running on the node.
- **Process-Tree Kill & Timeout**:
  - `--timeout <duration>` (enforced on the node) or `dl job kill <job_id>` terminates the entire process tree (Unix process group `pgid` / Windows Job Object), killing children and grandchildren.
- **Status, Offline Continuation & Log Resume**:
  - `dl job status <job_id>` prints `job_id=<job_id> device=<device> state=<running|completed|failed|killed|timed_out> exit_code=<int|none> bytes=<int>` (or `{"job_id":...,"device":...,"state":...,"exit_code":<int|null>,"bytes":<int>}` with `--json`).
  - If the node loses network mid-job, the local process keeps running and spooling output to disk; after reconnect, the controller inspects completion via `dl job status <job_id>` and resumes reading raw output bytes via `dl job logs <job_id> --offset <bytes>`.
  - `dl exec --wait-online <duration>` waits up to `<duration>` for a registered offline (`missed`) device to reconnect before dispatching; if still offline, fails non-zero with an `offline` error (`503`), distinct from unknown device (`404`), unauthorized (`401`/`403`), timeout (`124`), and command non-zero exit.
  - All controller `/v1/...` routes for `exec` and `job` reject `device_token` with `401`/`403` fail-closed.

## 4. File Transfer (`<= 500MB`, Stage 3)

- **CLI Surface**:
  - `dl cp [controller-flags] [--exclude <pattern>]... [--wait <duration>] [--detach] [from:]<src>... [to:]<target_dir>`
  - `dl transfer status [controller-flags] [--json] <transfer_id>`
  - `dl transfer cancel [controller-flags] <transfer_id>`
  - Flags (`--exclude`, `--wait`, `--detach`, `--json`, plus controller flags `--hub`, `--token`, `--ca`, `--state-dir`) are accepted before, between, or after positional arguments.
- **Endpoint & Path Model (`from + sources[] -> to + target_dir`)**:
  - Requires at least two positional arguments: one or more `<src>` paths followed by `<target_dir>`.
  - Endpoint syntax: a path with no `<device>:` prefix, a `local:<path>` prefix, or a Windows drive path (`C:\...` / `C:/...`) refers to the controller (`local`); `<device>:<path>` refers to remote device `<device>`.
  - All `<src>...` arguments must resolve to the same `from` endpoint (mixing source endpoints fails non-zero). At least one of `from` or `to` must be a remote device (`local -> local` fails non-zero); supports `A->B`, `local->B`, and `A->local`.
  - Every `<src>` (single file, directory, or multiple paths) is copied by its base name (`filepath.Base`) into `<target_dir>` (`<target_dir>/<base>`), creating `<target_dir>` if needed.
  - `--exclude <pattern>` (repeatable) matches glob patterns against each entry's base name or slash-normalized relative path, skipping matching files and directory subtrees during both pre-flight sizing and transfer.
- **Pre-Flight `<= 500MB` Scan**:
  - Before opening any HTTP `tar` stream or writing to `<target_dir>`, the source sums non-excluded regular file sizes.
  - If total size exceeds `500 MiB` (`524,288,000` bytes), `dl cp` fails non-zero before streaming any payload and prints an error to stderr containing `exceeds 500MB limit` and listing the largest offending files (relative path and byte size, largest first).
- **Same-Name SHA-256 Skip & `+x` Permission Bits**:
  - Same-name regular files under `<target_dir>` are compared by SHA-256 before streaming file bytes: identical files are skipped without streaming payload bytes or rewriting target disk (`skipped` count increments); missing or differing files are written/overwritten while preserving executable bits (`0755` when source has any `+x` bit, `0644` otherwise).
- **Path-Traversal & Symlink Escape Defense**:
  - Source scan and target extraction block `..` path segments, absolute archive paths, symlinks whose target resolves outside the source root or `<target_dir>`, and writes through pre-existing symlinks under `<target_dir>` that resolve outside `<target_dir>`, failing non-zero before any out-of-bounds disk write.
- **Zero-Disk Hub HTTP `tar` Relay, Concurrency & Lifecycle**:
  - File payloads stream via independent HTTP `tar` requests piped in-memory through the Hub (zero Hub disk spool files), never blocking the WebSocket control channel or concurrent `dl exec` on the same node during slow/throttled transfers.
  - Synchronous completion within `--wait` (default `30s`, when `--detach` is not set) prints `transfer_id=<id> state=completed files=<copied> skipped=<skipped> bytes=<transferred>/<total>` to stdout and exits `0`.
  - On `A->B` (remote-to-remote), `--detach` or exceeding `--wait` prints `transfer_id=<id>` to stdout and exits `0` while nodes `A` and `B` continue streaming via the Hub (`--detach` on `local->B` or `A->local` fails non-zero).
  - `dl transfer status <transfer_id>` prints `transfer_id=<id> from=<from> to=<to> state=<running|completed|failed|cancelled> files=<copied> skipped=<skipped> bytes=<transferred>/<total>` (or `{"transfer_id":...,"from":...,"to":...,"state":...,"files":<int>,"skipped":<int>,"bytes":<int>,"total_bytes":<int>}` with `--json`).
  - `dl transfer cancel <transfer_id>` aborts an in-flight transfer (`state=cancelled`, exit `0`). Unknown `<transfer_id>` fails non-zero (`404`), offline source/target device fails non-zero (`503` `offline`), and all controller `/v1/transfers...` routes reject `device_token` with `401`/`403` fail-closed.

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
- **Stage 4**: OS background services (`launchd` / `systemd` / Windows Service), one-line install/join script, and GitHub Actions 3-OS CI & 6-target Release pipeline:
  - **CLI Surface (`dl join --service`)**:
    - `dl join [--service] --hub <url> [--code <code>] [--ca <path>] [--state-dir <dir>]`
    - When `--service` is passed, `dl join` first resolves absolute paths for the `dl` executable, `--state-dir`, and `--ca` (when set), and ensures `<state-dir>/node.json` (`0600`) exists by exchanging `--code` up front if not yet enrolled (failing non-zero before touching OS service files if `--hub` is missing/invalid or `--code` is rejected).
    - The registered service command runs `<abs-dl> join --hub <url> --state-dir <abs-state-dir> [--ca <abs-ca-path>]` without embedding the one-time `--code` secret, then prints `dl service installed: platform=<os> unit=<path-or-name>` to stdout and exits `0`:
      - **macOS (`darwin`, `launchd`)**: writes plist `io.devicelink.node.plist` (`0644`, `RunAtLoad=true`, `KeepAlive=true`, logs in `<state-dir>/service.log`) to `/Library/LaunchDaemons/io.devicelink.node.plist` when `euid == 0` or `$HOME/Library/LaunchAgents/io.devicelink.node.plist` otherwise, and loads/starts it via `launchctl` (`bootout` ignore error, `bootstrap`, `kickstart -k`).
      - **Linux (`linux`, `systemd`)**: writes unit `device-link.service` (`0644`, `Restart=always`, `RestartSec=2`) to `/etc/systemd/system/device-link.service` (`systemctl daemon-reload` + `systemctl enable --now device-link.service`) when `euid == 0`, or `${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user/device-link.service` (`systemctl --user daemon-reload` + `systemctl --user enable --now device-link.service`) otherwise.
      - **Windows (`windows`, Windows Service)**: registers/updates service `DeviceLink` (`start= auto`) via `sc.exe` (`create` or `config`, then `start DeviceLink`), and when `dl join` starts under the Windows Service Control Manager (`svc.IsWindowsService()`), runs `node.Run` inside a `golang.org/x/sys/windows/svc` handler that cancels context cleanly on `Stop`/`Shutdown`.
  - **One-Line Install & Join Script (`scripts/install.sh`)**:
    - Detects `darwin`/`linux` and `amd64`/`arm64`, downloads `dl_<os>_<arch>.tar.gz` and `checksums.txt` from `${DL_BASE_URL}` (defaulting to GitHub Releases for `${DL_REPO:-12bitsD/device-link}` and `${DL_VERSION:-latest}`), verifies SHA-256 against `checksums.txt` (failing non-zero on mismatch), installs `dl` (`0755`) into `${DL_INSTALL_DIR}` (`/usr/local/bin` if writable, else `$HOME/.local/bin`), cleans temporary files on exit, and if trailing arguments are given (optional leading `--` stripped), execs `"$DL_INSTALL_DIR/dl" "$@"`.
  - **GitHub Actions CI & Release (`.github/workflows/ci.yml`, `.github/workflows/release.yml`)**:
    - `ci.yml` runs on `push` (`main`) and `pull_request`, executing static/style checks + 6-target cross-compile and running `go test -race` + `./test/e2e` across `ubuntu-latest`, `macos-latest`, and `windows-latest`.
    - `release.yml` runs on `v*` tags (and `workflow_dispatch`), verifies tests, builds `CGO_ENABLED=0` static archives for all 6 targets (`darwin`/`linux` `.tar.gz` and `windows` `.zip` × `amd64`/`arm64`) plus `checksums.txt`, and publishes them to GitHub Releases.

## 7. Black-Box Verification Contract

- Tests exercise the real compiled `./dl` binary in isolated multi-process setups (`--state-dir` per process, ephemeral localhost ports).
- Local TCP fault/throttle proxy simulates network drops (3s drop -> `missed`, offline job continuation & resume) and throttled transfers concurrent with `exec`.
- Every E2E test asserts zero lingering child processes, zero leaked temp files, and zero unexpected Hub disk files on teardown, and passes `go test -race -count=3 ./test/e2e` without flakes.
- Cross-platform verification covers `CGO_ENABLED=0` builds across `darwin/linux/windows` (`amd64/arm64`), cross-compiled E2E test binaries for `linux` and `windows`, local Linux container (`colima`/`docker`) node-to-host-Hub interop, and GitHub Actions on `ubuntu-latest`, `macos-latest`, and `windows-latest`.
