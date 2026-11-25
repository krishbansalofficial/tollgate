#!/usr/bin/env bash
# Chaos scenarios against the compose stack. Prints observed behaviour;
# results are written up by hand in loadtest/CHAOS.md.
#
#   loadtest/chaos.sh [redis|provider|kill|all]
set -uo pipefail
export MSYS_NO_PATHCONV=1
cd "$(dirname "$0")/.."

P=tollgate-chaos
DC="docker compose -p $P -f compose.yaml -f loadtest/compose.chaos.yaml"
URL=http://localhost:8080/v1/chat/completions
BODY='{"model":"mock/small","messages":[{"role":"user","content":"chaos"}]}'

up() { FAIL_MODE=${1:-open} $DC up -d --build --wait >/dev/null 2>&1 || $DC up -d >/dev/null; sleep 3; }
key() { $DC run --rm --no-deps tollgate keys create --project chaos --rpm 100000 --tpm 100000000 2>/dev/null | tr -d '\r'; }
burst() { # burst N body: print status-code histogram and provider headers
  local n=$1 body=$2
  for _ in $(seq "$n"); do
    curl -s -o /dev/null -D - -m 15 "$URL" -H "Authorization: Bearer $KEY" -d "$body" |
      tr -d '\r' | awk 'NR==1{s=$2} tolower($1)=="x-tollgate-provider:"{p=$2} END{print s, (p?p:"-")}'
  done | sort | uniq -c
}
metric() { curl -s localhost:9091/api/v1/query --data-urlencode "query=$1" | python -c 'import sys,json; [print("   ",r["metric"],r["value"][1]) for r in json.load(sys.stdin)["data"]["result"]]'; }
ledger() { $DC exec -T postgres psql -U tollgate -tAc "$1"; }
mock() { # mock <service> <args...>: restart a mock with new flags
  local svc=$1; shift
  $DC rm -sf "$svc" >/dev/null 2>&1
  docker rm -f "${P}-${svc}-x" >/dev/null 2>&1
  $DC run -d --no-deps --name "${P}-${svc}-x" --use-aliases --entrypoint /usr/local/bin/mockupstream "$svc" -addr :8081 "$@" >/dev/null
  sleep 1
}

scenario_redis() {
  echo "### Redis down"
  for mode in open closed; do
    $DC down -v --remove-orphans >/dev/null 2>&1; docker rm -f "${P}-mockupstream-x" >/dev/null 2>&1; up "$mode"; KEY=$(key)
    echo "-- fail mode: $mode; healthy:"; burst 5 "$BODY"
    $DC stop redis >/dev/null 2>&1
    echo "-- fail mode: $mode; Redis stopped:"; burst 20 "$BODY"
    t0=$(date +%s%N); curl -s -o /dev/null "$URL" -H "Authorization: Bearer $KEY" -d "$BODY"; echo "   one request latency: $(( ($(date +%s%N)-t0)/1000000 )) ms"
    echo "   readyz: $(curl -s -o /dev/null -w '%{http_code}' localhost:8080/readyz)"
    $DC logs tollgate 2>&1 | grep -c "rate limiter degraded" | sed 's/^/   degraded log lines: /'
    $DC start redis >/dev/null 2>&1; sleep 3
    echo "-- fail mode: $mode; Redis back:"; burst 5 "$BODY"
  done
}

scenario_provider() {
  echo "### Primary provider failing (fallback = ollama -> mockbackup)"
  $DC down -v --remove-orphans >/dev/null 2>&1; docker rm -f "${P}-mockupstream-x" >/dev/null 2>&1; up open; KEY=$(key)
  for flags in "-error-rate 1" "-429-rate 1" "-error-rate 0.3"; do
    mock mockupstream -latency 20ms $flags
    $DC restart tollgate >/dev/null 2>&1; sleep 4 # fresh breaker per phase
    ledger "truncate usage_events" >/dev/null
    echo "-- primary: $flags (40 requests): status, provider"; burst 40 "$BODY"
    sleep 2
    echo "   ledger: provider | status | requests | mean attempts | p50 latency ms"
    ledger "select provider, status, count(*), round(avg(attempts),2), percentile_cont(0.5) within group (order by latency_ms) from usage_events group by 1,2 order by 1,2" | sed 's/^/   /'
    echo "   breaker log: $($DC logs tollgate --since 30s 2>&1 | grep -o '"state":"[a-z_]*"' | tr '
' ' ')"
  done
}

scenario_kill() {
  echo "### Gateway killed mid-stream (20 chunks x 400 ms = ~8 s stream)"
  SB='{"model":"mock/small","stream":true,"messages":[{"role":"user","content":"long"}]}'
  for sig in SIGTERM SIGKILL; do
    $DC down -v --remove-orphans >/dev/null 2>&1; docker rm -f "${P}-mockupstream-x" >/dev/null 2>&1; up open; KEY=$(key)
    before=$(ledger "select count(*) from usage_events")
    out=$(mktemp)
    curl -sN -m 30 "$URL" -H "Authorization: Bearer $KEY" -d "$SB" >"$out" 2>&1 &
    cpid=$!
    sleep 3
    docker kill --signal "$sig" "$($DC ps -q tollgate)" >/dev/null
    wait $cpid; rc=$?
    echo "-- $sig at ~3 s: curl exit=$rc, content chunks=$(grep -c '"content"' "$out"), ended with [DONE]=$(grep -q '\[DONE\]' "$out" && echo yes || echo no)"
    sleep 2; $DC start tollgate >/dev/null 2>&1; sleep 4
    echo "   ledger rows written for the stream: $(( $(ledger "select count(*) from usage_events") - before ))"
    $DC logs tollgate 2>&1 | grep -E "shutdown: (draining|complete)" | tail -2 | cut -c1-200 | sed 's/^/   /'
    rm -f "$out"
  done
}

case ${1:-all} in
  redis) scenario_redis ;;
  provider) scenario_provider ;;
  kill) scenario_kill ;;
  all) scenario_redis; scenario_provider; scenario_kill ;;
esac
$DC down -v --remove-orphans >/dev/null 2>&1; docker rm -f "${P}-mockupstream-x" >/dev/null 2>&1
