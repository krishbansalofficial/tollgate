#!/usr/bin/env bash
# Step-load runner: for each rate, run k6 for DURATION, save the k6 summary,
# and read the gateway's own overhead quantiles from Prometheus for the same
# window. Output: loadtest/results/<label>/{summary-*.json,steps.tsv}.
#
#   KEY=tg_... loadtest/run.sh <label> <mode> <rate> [rate...]
set -euo pipefail
export MSYS_NO_PATHCONV=1 # Git Bash: don't rewrite container paths

LABEL=$1 MODE=$2
shift 2
DURATION=${DURATION:-45s}
NET=${NET:-tollgate-lt_default}
PROM=${PROM:-http://127.0.0.1:9091}
HERE=$(cd "$(dirname "$0")" && { pwd -W 2>/dev/null || pwd; }) # pwd -W: C:/... path that both Docker and Windows Python accept
OUT=$HERE/results/$LABEL
mkdir -p "$OUT"
win=${DURATION%s}

q() { # q <quantile>: overhead quantile in ms over the last step window
  curl -s "$PROM/api/v1/query" --data-urlencode \
    "query=histogram_quantile($1, sum by (le) (rate(tollgate_overhead_seconds_bucket[${win}s])))*1000" |
    python -c 'import sys,json; r=json.load(sys.stdin)["data"]["result"]; print(round(float(r[0]["value"][1]),3) if r else "nan")'
}

[[ -f $OUT/steps.tsv ]] || printf 'mode\ttarget_rps\tachieved_rps\tfailed_pct\tk6_p50_ms\tk6_p95_ms\tk6_p99_ms\toverhead_p50_ms\toverhead_p99_ms\n' >"$OUT/steps.tsv"
for RATE in "$@"; do
  echo "== $MODE @ $RATE rps"
  docker run --rm --network "$NET" -v "$HERE:/lt" -e KEY -e MODE="$MODE" -e RATE="$RATE" -e DURATION="$DURATION" \
    grafana/k6:latest run --quiet --summary-export "/lt/results/$LABEL/summary-$MODE-$RATE.json" /lt/chat.js >/dev/null 2>&1 || true
  sleep 16 # let Prometheus scrape the tail of the step (15 s interval)
  python - "$OUT/summary-$MODE-$RATE.json" "$MODE" "$RATE" "$(q 0.5)" "$(q 0.99)" >>"$OUT/steps.tsv" <<'PY'
import json, sys
path, mode, rate, o50, o99 = sys.argv[1:]
m = json.load(open(path))["metrics"]
d = m["http_req_duration"]
print("\t".join(map(str, [mode, rate, round(m["http_reqs"]["rate"], 1), round(100 * m["http_req_failed"]["value"], 2),
                           round(d["med"], 2), round(d["p(95)"], 2), round(d["p(99)"], 2), o50, o99])))
PY
  tail -1 "$OUT/steps.tsv"
  sleep 5
done
