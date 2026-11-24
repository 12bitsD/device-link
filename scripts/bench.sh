#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."
out_dir=".bench"
mkdir -p "$out_dir"

echo "==> build binary"
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$out_dir/dl" ./cmd/dl
size_bytes=$(wc -c < "$out_dir/dl" | tr -d ' ')
echo "binary_size_bytes=$size_bytes"

echo "==> code size (non-test / test Go LOC)"
src_loc=$(find ./cmd ./internal -name '*.go' ! -name '*_test.go' -print0 | xargs -0 cat | wc -l | tr -d ' ')
test_loc=$(find . -name '*_test.go' -not -path './.git/*' -not -path './.bench/*' -print0 | xargs -0 cat | wc -l | tr -d ' ')
echo "src_loc=${src_loc:-0}"
echo "test_loc=${test_loc:-0}"

echo "==> go benchmarks"
go test -run='^$' -bench=. -benchmem ./... | tee "$out_dir/bench.txt"
