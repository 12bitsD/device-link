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
src_loc=$(find ./cmd ./internal -name '*.go' ! -name '*_test.go' -not -path '*/testdata/*' -print0 | xargs -0 cat | wc -l | tr -d ' ')
test_loc=$(find ./cmd ./internal ./test -name '*_test.go' -not -path '*/testdata/*' -print0 | xargs -0 cat | wc -l | tr -d ' ')
echo "src_loc=${src_loc:-0}"
echo "test_loc=${test_loc:-0}"

{
  echo "binary_size_bytes=$size_bytes"
  echo "src_loc=${src_loc:-0}"
  echo "test_loc=${test_loc:-0}"
} > "$out_dir/latest.env"

if [ -f "$out_dir/baseline.env" ]; then
  echo "==> delta vs baseline.env"
  python3 - "$out_dir/baseline.env" "$out_dir/latest.env" <<'PY'
import sys

def load(path):
    return dict(line.strip().split("=", 1) for line in open(path) if "=" in line)

base, cur = load(sys.argv[1]), load(sys.argv[2])
for key in ("binary_size_bytes", "src_loc", "test_loc"):
    b, c = int(base.get(key, 0)), int(cur.get(key, 0))
    delta = c - b
    pct = (delta / b * 100) if b else 0.0
    print(f"{key}: {b} -> {c} ({delta:+d}, {pct:+.1f}%)")
PY
fi

echo "==> go benchmarks"
go test -run='^$' -bench=. -benchmem -benchtime=200ms ./internal/... | tee "$out_dir/bench.txt"
