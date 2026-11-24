# Device Link (`dl`)

Single-binary, single-trust-domain remote `exec` and streaming `<= 500MB` file transfer (`cp`) across macOS, Linux, Windows, and private Pods — built for both humans and AI agents.

- **Single static Go binary**: `darwin` / `linux` / `windows` × `amd64` / `arm64` (`CGO_ENABLED=0`).
- **Outbound double-NAT traversal**: nodes dial outbound to a self-hosted HTTPS Hub (1 WebSocket control channel + independent zero-disk HTTP `tar` streams).
- **Strict role separation**: node credentials (`device_token`) fail closed (`401`/`403`) on all controller endpoints; only `controller_token` holders manage devices or run `exec` / `cp`.
- **Resilient execution & transfer**: login `PATH` loading, process-tree termination (`pgid` / Windows Job Object), offline job log continuation with `--offset` resume, pre-flight `500MB` size guard, and SHA-256 skip.

See [docs/spec.md](docs/spec.md) for the v1 specification.

## Install

```sh
# Install latest binary (macOS / Linux)
curl -fsSL https://raw.githubusercontent.com/12bitsD/device-link/main/scripts/install.sh | sh

# One-line install + enroll as OS background service (launchd / systemd)
curl -fsSL https://raw.githubusercontent.com/12bitsD/device-link/main/scripts/install.sh | sh -s -- \
  join --service --hub https://hub.example:8443 --code <invite_code>
```

Or build from source:

```sh
CGO_ENABLED=0 go build -trimpath -o dl ./cmd/dl
```

## Quickstart

```sh
# 1. Start Hub
dl hub --addr :8443 --host hub.example --state-dir ~/.device-link/hub

# 2. Mint a one-time join command (controller)
dl invite --hub https://hub.example:8443 --name macbook

# 3. Join on the target machine (foreground or OS service: launchd / systemd / Windows Service)
dl join --hub https://hub.example:8443 --code <code> --ca cert.pem
dl join --service --hub https://hub.example:8443 --code <code> --ca cert.pem

# 4. Inspect & manage devices
dl ls
dl ttl macbook 24h
dl rm macbook

# 5. Remote exec & job lifecycle
dl exec macbook -- uname -a
dl exec --detach --timeout 10m macbook "make test"
dl job status <job_id>
dl job logs --offset 0 <job_id>
dl job kill <job_id>

# 6. Streaming file transfer (<= 500MB, local->B, A->local, A->B)
dl cp --exclude "*.log" ./build macbook:/opt/app
dl cp macbook:/var/log/app.log ./logs
dl cp --detach node-a:/srv/bundle node-b:/srv/out
dl transfer status <transfer_id>
dl transfer cancel <transfer_id>
```

## Verification

```sh
./scripts/verify.sh   # gofmt, vet, staticcheck, deadcode, Bits Go AST, -race unit & E2E (-count=3), 6-target cross-compile
./scripts/bench.sh    # binary size, LOC, baseline delta, Go benchmarks
```

## License

[MIT](LICENSE)
