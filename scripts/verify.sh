#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."
export PATH="$(go env GOPATH)/bin:$PATH"
tmp_dir=$(mktemp -d)
trap 'rm -rf "$tmp_dir"' EXIT

echo "==> gofmt"
unformatted=$(find . -name '*.go' -not -path './.git/*' -not -path './.bench/*' -not -path './.ryugi/*' -not -path '*/testdata/*' -print0 | xargs -0 gofmt -l)
if [ -n "$unformatted" ]; then
  echo "gofmt required:"
  echo "$unformatted"
  exit 1
fi

echo "==> go vet"
go vet ./...

echo "==> staticcheck"
staticcheck ./...

echo "==> deadcode (whole-program reachability from ./cmd/dl across darwin/linux/windows)"
GOBIN="$tmp_dir" go install golang.org/x/tools/cmd/deadcode@v0.41.0
for target_os in darwin linux windows; do
  unreachable=$(CGO_ENABLED=0 GOOS="$target_os" GOARCH=amd64 "$tmp_dir/deadcode" ./cmd/dl)
  if [ -n "$unreachable" ]; then
    echo "unreachable code detected on $target_os:"
    echo "$unreachable"
    exit 1
  fi
done

echo "==> Bits Go AST style + fixture self-tests"
go test -count=1 ./internal/stylecheck

echo "==> bits CLI audit (unsupported must only be Go files covered by AST gate)"
bits_help=""
if command -v bits >/dev/null 2>&1; then
  bits_help="$(bits style --help 2>/dev/null || true)"
fi
if [[ "$bits_help" == *"Usage: bits style"* ]]; then
  bits_report="$tmp_dir/bits.json"
  bits style report . --format json --output "$bits_report" >/dev/null
  python3 - "$bits_report" <<'PY'
import json
import sys

report = json.load(open(sys.argv[1]))
summary = report.get("summary", {})
if summary.get("violations", 0) > 0 or summary.get("scan_errors", 0) > 0:
    raise SystemExit(f"bits violations/scan errors: {summary}")
non_go = [p for p in report.get("unsupported", []) if not p.endswith(".go")]
if non_go:
    raise SystemExit(f"bits unsupported non-Go files (not covered by AST gate): {non_go}")
print(f"bits: {summary.get('files_scanned', 0)} scanned, {len(report.get('unsupported', []))} Go files delegated to AST gate")
PY
else
  echo "warning: bits CLI not installed; skipping bits audit"
fi

echo "==> go test -race (internal unit tests)"
CGO_ENABLED=1 go test -race -count=1 ./cmd/... ./internal/...

echo "==> go test -race -count=3 (blackbox E2E stability gate)"
CGO_ENABLED=1 go test -race -count=3 ./test/e2e

echo "==> cross-compile binaries & OS-specific E2E test suites (CGO_ENABLED=0)"
tmp_bin="$tmp_dir/bin"
mkdir -p "$tmp_bin"
for os in darwin linux windows; do
  for arch in amd64 arm64; do
    ext=""
    if [ "$os" = "windows" ]; then
      ext=".exe"
    fi
    echo "  building $os/$arch"
    CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -o "$tmp_bin/dl-$os-$arch$ext" ./cmd/dl
  done
done
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go test -c -o "$tmp_bin/e2e-linux.test" ./test/e2e
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go test -c -o "$tmp_bin/e2e-windows.test.exe" ./test/e2e

echo "==> verify OK"
