#!/bin/bash
# Measure time-to-first-token and total latency against the gateway.
#
#   ./scripts/bench.sh [model] [prompt] [iterations]
#
# TTFT is the timestamp of the first SSE chunk that carries content; total is
# the timestamp of [DONE]. Both are wall-clock, measured on the client side, so
# they include gateway overhead — which is the number that matters.
set -euo pipefail

BASE="${FMUX_BASE:-http://127.0.0.1:1976/v1}"
MODEL="${1:-qwen3.8:27b}"
PROMPT="${2:-用一句话说明什么是统一内存架构}"
ITERS="${3:-3}"

if ! curl -sf --max-time 3 "${BASE}/models" >/dev/null; then
  echo "error: no gateway at ${BASE}" >&2
  exit 1
fi

echo "model:  $MODEL"
echo "prompt: $PROMPT"
echo

printf "%-5s %10s %11s %7s\n" "run" "ttft(ms)" "total(ms)" "chunks"

payload=$(python3 -c '
import json, sys
print(json.dumps({
    "model": sys.argv[1],
    "stream": True,
    "stream_options": {"include_usage": True},
    "messages": [{"role": "user", "content": sys.argv[2]}],
}))' "$MODEL" "$PROMPT")

for i in $(seq 1 "$ITERS"); do
  tmp=$(mktemp)
  start=$(python3 -c 'import time;print(time.time())')

  curl -sN --max-time 600 -X POST "${BASE}/chat/completions" \
    -H 'Content-Type: application/json' -d "$payload" >"$tmp" || true

  python3 -c '
import json, sys, time

path, start, idx = sys.argv[1], float(sys.argv[2]), sys.argv[3]
ttft = None
chunks = 0

with open(path, "r", errors="replace") as fh:
    for line in fh:
        if not line.startswith("data: "):
            continue
        body = line[6:].strip()
        if body == "[DONE]":
            break
        chunks += 1
        if ttft is None:
            try:
                obj = json.loads(body)
            except Exception:
                continue
            for choice in obj.get("choices") or []:
                if (choice.get("delta") or {}).get("content"):
                    ttft = time.time()
                    break

total = time.time()
print("%-5s %10s %11s %7d" % (
    idx,
    "%.0f" % ((ttft - start) * 1000) if ttft else "-",
    "%.0f" % ((total - start) * 1000),
    chunks,
))' "$tmp" "$start" "$i"

  rm -f "$tmp"
done
