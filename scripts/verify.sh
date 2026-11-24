#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."
export PATH="$(go env GOPATH)/bin:$PATH"

echo "==> gofmt"
unformatted=$(find . -name '*.go' -not -path './.git/*' -not -path './.bench/*' -print0 | xargs -0 gofmt -l)
if [ -n "$unformatted" ]; then
  echo "gofmt required:"
  echo "$unformatted"
  exit 1
fi

echo "==> go vet"
go vet ./...

echo "==> staticcheck"
staticcheck ./...

echo "==> go test -race (unit + AST style + blackbox E2E)"
CGO_ENABLED=1 go test -race -count=1 ./...

echo "==> cross-compile (CGO_ENABLED=0)"
tmp_bin=$(mktemp -d)
trap 'rm -rf "$tmp_bin"' EXIT
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

echo "==> verify OK"
