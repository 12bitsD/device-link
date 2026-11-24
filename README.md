# Device Link (`dl`)

Ultra-lightweight, single-binary remote shell and file transfer for your own machines and Pods — built for both humans and agents.

- **Single static Go binary**: runs on macOS, Linux, and Windows (`amd64` / `arm64`, zero CGO).
- **Works through double NAT**: devices dial outbound to a tiny self-hosted Hub over one HTTPS port (WebSocket control + in-memory streaming).
- **Strict role isolation**: controlled nodes only accept commands; only controller-token holders can list or manage devices.
- **Paste-to-connect**: one-time invite codes, permanent-by-default device credentials, automatic name deduplication (`mac` → `mac-2`).
- **Agent-friendly surface**: `exec` with resumable logs across network drops, plus `<= 500MB` zero-spool `tar` file copies between any two machines.

> Status: early development. See [docs/spec.md](docs/spec.md) for the v1 contract and rollout stages.

## Quickstart (Stage 1)

```sh
go build -o dl ./cmd/dl

# 1. Start the Hub on a reachable host
./dl hub --addr :8443 --state-dir ~/.dl-hub

# 2. Mint a one-time join command from your controller
./dl invite --hub https://hub.example:8443 --name macbook

# 3. Paste the printed `dl join ...` command on the target machine, then:
./dl ls --hub https://hub.example:8443
```

## Development

```sh
./scripts/verify.sh   # format, vet, staticcheck, AST style rules, race tests, 6-OS/arch builds
./scripts/bench.sh    # binary size, LOC, benchmarks
```

## License

[MIT](LICENSE)
