#!/usr/bin/env bash
# picocdn end-to-end self-test.
#
# Runs: gofmt, go vet, go test, go test -race, microbenchmarks, build,
# CLI smoke, HTTP smoke via path-fallback (no DNS required), an optional
# subdomain-mode section, GC/backup/restore, and optional load tests
# (bombardier, vegeta) if those tools exist.
#
# Exits 0 only if every check passes.

set -uo pipefail

REPO=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$REPO"

# ---------- options ----------
SKIP_LOAD=0
SKIP_BENCH=0
SKIP_RACE=0
KEEP_WORK=0
PORT=${PICOCDN_SELFTEST_PORT:-$((19000 + RANDOM % 1000))}

usage() {
  cat <<EOF
Usage: $0 [options]

  --no-load    skip bombardier/vegeta load tests
  --no-bench   skip Go microbenchmarks
  --no-race    skip 'go test -race'
  --keep       do not delete work dir on exit (for debugging)
  --port N     HTTP port to bind on 127.0.0.1 (default: random in 19000-19999)
  -h, --help   show this help
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --no-load)  SKIP_LOAD=1 ;;
    --no-bench) SKIP_BENCH=1 ;;
    --no-race)  SKIP_RACE=1 ;;
    --keep)     KEEP_WORK=1 ;;
    --port)     shift; PORT="$1" ;;
    -h|--help)  usage; exit 0 ;;
    *) echo "unknown flag: $1" >&2; usage >&2; exit 2 ;;
  esac
  shift
done

# ---------- colors ----------
if [[ -t 1 ]]; then
  GREEN=$'\033[0;32m'; RED=$'\033[0;31m'; YELLOW=$'\033[0;33m'
  BLUE=$'\033[0;34m'; BOLD=$'\033[1m'; NC=$'\033[0m'
else
  GREEN=''; RED=''; YELLOW=''; BLUE=''; BOLD=''; NC=''
fi

# ---------- state ----------
FAIL=0
SERVER_PID=
WORK=$(mktemp -d -p "${TMPDIR:-/tmp}" picocdn-selftest-XXXX)
BIN="$WORK/picocdn"

cleanup() {
  local rc=$?
  if [[ -n "${SERVER_PID:-}" ]] && kill -0 "$SERVER_PID" 2>/dev/null; then
    kill "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
  fi
  if [[ "$KEEP_WORK" == "1" ]]; then
    echo "${YELLOW}keeping work dir:${NC} $WORK"
  else
    rm -rf "$WORK"
  fi
  exit $rc
}
trap cleanup EXIT INT TERM

# ---------- pretty printing ----------
SECTION_NUM=0
STEP_NUM=0
section() {
  SECTION_NUM=$((SECTION_NUM + 1))
  printf "\n${BOLD}== %d. %s ==${NC}\n" "$SECTION_NUM" "$1"
}
step() {
  STEP_NUM=$((STEP_NUM + 1))
  printf "  [%02d] %-58s " "$STEP_NUM" "$1"
}
ok()   { printf "${GREEN}PASS${NC}\n"; }
ko()   { printf "${RED}FAIL${NC}\n       ↳ %s\n" "$*"; FAIL=$((FAIL + 1)); }
info() { printf "       ${BLUE}%s${NC}\n" "$*"; }

# ---------- helpers ----------

# expect <actual> <expected> [hint]
expect() {
  if [[ "$1" == "$2" ]]; then ok; else ko "want=$2 got=$1 ${3:-}"; fi
}

# http_status <curl args...>
http_status() {
  curl -s -o /dev/null -w '%{http_code}' --max-time 10 "$@" 2>/dev/null || echo "curl_failed"
}

# read_json_field <json> <field>
read_json_field() {
  echo "$1" | awk -F\" '/"'"$2"'":/ {print $4; exit}'
}

# start_server: launches picocdn without -base-domain (path-fallback only).
# Extra flags can be appended.
start_server() {
  _start_server "" "$@"
}

# start_server_subdomain: launches picocdn with -base-domain example.test so the
# subdomain dispatcher is active.
start_server_subdomain() {
  _start_server "example.test" "$@"
}

_start_server() {
  local base="$1"; shift
  local -a flags=( -addr "127.0.0.1:$PORT" -data-dir ./data -auth-file ./auth.json )
  if [[ -n "$base" ]]; then
    flags+=( -base-domain "$base" )
  fi
  flags+=( "$@" )
  cd "$WORK"
  ( "$BIN" serve "${flags[@]}" >./srv.out 2>./srv.err ) &
  SERVER_PID=$!
  for _ in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15; do
    if curl -fsS --max-time 1 "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1; then
      return 0
    fi
    if ! kill -0 "$SERVER_PID" 2>/dev/null; then
      echo "${RED}server crashed during startup${NC}" >&2
      cat ./srv.err >&2
      return 1
    fi
    sleep 0.1
  done
  echo "${RED}server did not become healthy${NC}" >&2
  cat ./srv.err >&2 || true
  return 1
}

stop_server() {
  if [[ -n "${SERVER_PID:-}" ]] && kill -0 "$SERVER_PID" 2>/dev/null; then
    kill "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
  fi
  SERVER_PID=
}

# Find an installed load-test tool; prefer PATH then ~/.local/bin.
find_tool() {
  local name="$1"
  if command -v "$name" >/dev/null 2>&1; then
    command -v "$name"
  elif [[ -x "$HOME/.local/bin/$name" ]]; then
    echo "$HOME/.local/bin/$name"
  fi
}

# ---------- 1. static checks ----------
section "static checks"

step "go is on PATH"
if command -v go >/dev/null 2>&1; then ok; info "$(go version)"
else ko "go binary not found"; exit 1; fi

step "gofmt -l reports no diffs"
out=$(gofmt -l cmd internal 2>&1)
if [[ -z "$out" ]]; then ok; else ko "$out"; fi

step "go vet ./..."
if go vet ./... >"$WORK/vet.log" 2>&1; then ok
else ko "see $WORK/vet.log"; fi

# ---------- 2. unit tests ----------
section "unit tests"

step "go test ./... -count=1"
if go test -count=1 ./... >"$WORK/test.log" 2>&1; then ok
else ko "see $WORK/test.log"; fi

if [[ "$SKIP_RACE" == "0" ]]; then
  step "go test -race -count=1 ./..."
  if go test -race -count=1 ./... >"$WORK/race.log" 2>&1; then ok
  else ko "see $WORK/race.log"; fi
fi

# ---------- 3. microbenchmarks ----------
if [[ "$SKIP_BENCH" == "0" ]]; then
  section "microbenchmarks"
  step "go test -bench=. -benchtime=200ms"
  if go test -bench=. -benchmem -run=^$ -benchtime=200ms ./... >"$WORK/bench.log" 2>&1; then
    ok
    grep -E '^Benchmark' "$WORK/bench.log" | sed 's/^/       /'
  else
    ko "see $WORK/bench.log"
  fi
fi

# ---------- 4. build ----------
section "build"

step "go build ./cmd/picocdn"
if go build -o "$BIN" ./cmd/picocdn 2>"$WORK/build.err"; then ok
else ko "see $WORK/build.err"; exit 1; fi

cd "$WORK"
mkdir -p data

# ---------- 5. CLI smoke ----------
section "CLI smoke"

step "namespace create default"
out=$("$BIN" namespace create --auth-file ./auth.json default 2>&1)
TOKEN=$(read_json_field "$out" token)
if [[ -n "$TOKEN" ]]; then ok; else ko "no token in: $out"; fi

step "auth.json has mode 0600"
mode=$(stat -c '%a' ./auth.json 2>/dev/null)
expect "$mode" "600"

step "auth.json contains no plaintext token"
if grep -qF "$TOKEN" ./auth.json 2>/dev/null; then
  ko "plaintext token leaked into auth.json"
else
  ok
fi

step "namespace list shows default"
out=$("$BIN" namespace list --auth-file ./auth.json 2>&1)
if echo "$out" | grep -q '"name": "default"'; then ok; else ko "$out"; fi

step "namespace show prints owner_token_id"
out=$("$BIN" namespace show --auth-file ./auth.json default 2>&1)
if echo "$out" | grep -q '"owner_token_id"'; then ok; else ko "$out"; fi

step "token create with read perm"
out=$("$BIN" token create --auth-file ./auth.json --name reader --perm read default 2>&1)
READ_TOKEN=$(read_json_field "$out" token)
if [[ -n "$READ_TOKEN" ]]; then ok; else ko "$out"; fi

step "token list shows 2 tokens"
n=$("$BIN" token list --auth-file ./auth.json default 2>&1 | grep -c '"id":')
expect "$n" "2"

step "namespace set-public on"
out=$("$BIN" namespace set-public --auth-file ./auth.json --on default 2>&1)
if echo "$out" | grep -q '"public_read": true'; then ok; else ko "$out"; fi

step "namespace set-public off"
out=$("$BIN" namespace set-public --auth-file ./auth.json --off default 2>&1)
if echo "$out" | grep -q '"public_read": false'; then ok; else ko "$out"; fi

step "namespace create rejects uppercase"
if "$BIN" namespace create --auth-file ./auth.json BadName >/dev/null 2>&1; then
  ko "expected failure for uppercase namespace"
else
  ok
fi

step "namespace create rejects dotted name"
if "$BIN" namespace create --auth-file ./auth.json bad.name >/dev/null 2>&1; then
  ko "expected failure for dotted namespace"
else
  ok
fi

step "token revoke removes one token"
TID=$("$BIN" token list --auth-file ./auth.json default 2>&1 | awk -F\" '/"id":/ {print $4}' | tail -1)
"$BIN" token revoke --auth-file ./auth.json default "$TID" >/dev/null 2>&1
n=$("$BIN" token list --auth-file ./auth.json default 2>&1 | grep -c '"id":')
expect "$n" "1"

step "owner token revoke is refused"
OWNER_ID=$("$BIN" token list --auth-file ./auth.json default 2>&1 | awk -F\" '/"id":/ {print $4}' | head -1)
if "$BIN" token revoke --auth-file ./auth.json default "$OWNER_ID" >/dev/null 2>&1; then
  ko "owner token should not be revokable"
else
  ok
fi

# Re-issue a read token for HTTP tests.
out=$("$BIN" token create --auth-file ./auth.json --name reader --perm read default 2>&1)
READ_TOKEN=$(read_json_field "$out" token)

# ---------- 6. HTTP smoke (path-fallback, no base-domain) ----------
section "HTTP smoke — path-fallback (no DNS, no -base-domain)"

if start_server -reload-interval 300ms -max-upload-bytes 1048576; then
  info "server PID=$SERVER_PID port=$PORT max-upload=1MB reload=300ms base-domain=<none>"
else
  ko "failed to start server"; exit 1
fi

# Recipes use the local 'cdn' helper, as documented in README.
cdn() { curl -fsS -H "Authorization: Bearer $TOKEN" "$@"; }
HOST="http://127.0.0.1:$PORT/default"

step "GET /healthz returns 200"
expect "$(http_status "http://127.0.0.1:$PORT/healthz")" "200"

echo 'hello picocdn' > ./hello.txt
step "PUT upload returns 201"
up=$(cdn -T hello.txt -H 'Content-Type: text/plain' "$HOST/docs/hello.txt" 2>/dev/null) || up=""
HASH=$(echo "$up" | grep -oE '"hash":"[0-9a-f]{64}"' | head -1 | tr -d '"' | sed 's/hash://')
if [[ -n "$HASH" ]]; then ok; else ko "no hash; body: $up"; fi

step "GET returns body"
body=$(cdn "$HOST/docs/hello.txt" 2>/dev/null)
if [[ "$body" == "hello picocdn" ]]; then ok; else ko "got: $body"; fi

step "single-segment path"
echo 'one-segment' > ./one.txt
cdn -T one.txt "$HOST/one.txt" >/dev/null
body=$(cdn "$HOST/one.txt" 2>/dev/null)
if [[ "$body" == "one-segment" ]]; then ok; else ko "got: $body"; fi

step "HEAD exposes ETag and Content-Length"
hdrs=$(cdn -I "$HOST/docs/hello.txt" 2>/dev/null)
if echo "$hdrs" | grep -qiE "^etag: \"sha256:$HASH\"" && \
   echo "$hdrs" | grep -qiE "^content-length:"; then
  ok
else
  ko "missing headers; got: $(echo "$hdrs" | tr -d '\r')"
fi

step "Range bytes=0-4 returns first 5 bytes"
body=$(cdn -H "Range: bytes=0-4" "$HOST/docs/hello.txt" 2>/dev/null)
if [[ "$body" == "hello" ]]; then ok; else ko "got: $body"; fi

step 'If-None-Match returns 304'
expect "$(http_status -H "Authorization: Bearer $TOKEN" \
  -H "If-None-Match: \"sha256:$HASH\"" \
  "$HOST/docs/hello.txt")" "304"

step "list at /{namespace} returns objects"
n=$(cdn "$HOST" 2>/dev/null | grep -oc '"hash":"[0-9a-f]\{64\}"')
if [[ "$n" -ge 1 ]]; then ok; else ko "no entries (n=$n)"; fi

step "list with ?prefix= narrows results"
n=$(cdn "$HOST?prefix=/docs" 2>/dev/null | grep -oc '"hash":"[0-9a-f]\{64\}"')
if [[ "$n" -ge 1 ]]; then ok; else ko "no entries (n=$n)"; fi

# ---- auth checks ----
step "missing token returns 401"
expect "$(http_status "$HOST/docs/hello.txt")" "401"

step "wrong token returns 403"
expect "$(http_status -H "Authorization: Bearer wrong" "$HOST/docs/hello.txt")" "403"

step "read-only token cannot PUT (403)"
expect "$(http_status -X PUT --data-binary @hello.txt -H "Authorization: Bearer $READ_TOKEN" \
  "$HOST/x.txt")" "403"

step "read-only token cannot DELETE (403)"
expect "$(http_status -X DELETE -H "Authorization: Bearer $READ_TOKEN" \
  "$HOST/docs/hello.txt")" "403"

step "PUT to nonexistent namespace returns 404"
expect "$(http_status -X PUT --data-binary @hello.txt -H "Authorization: Bearer $TOKEN" \
  "http://127.0.0.1:$PORT/missing/x.txt")" "404"

# ---- path traversal ----
step "encoded %2e%2e traversal rejected (400)"
expect "$(http_status --path-as-is -H "Authorization: Bearer $TOKEN" \
  "$HOST/docs/%2e%2e/etc/passwd")" "400"

# Note: literal ".." in path-fallback URLs is normalized by Go's ServeMux
# *before* our handler runs (307 redirect to the cleaned path), so we only
# verify literal-.. rejection in the subdomain section below.

step "PUT with traversal path rejected (400)"
expect "$(http_status -X PUT --path-as-is --data-binary @hello.txt \
  -H "Authorization: Bearer $TOKEN" \
  "$HOST/docs/%2e%2e/secret.txt")" "400"

# ---- routing edge cases ----
step "POST on object path returns 405"
expect "$(http_status -X POST -H "Authorization: Bearer $TOKEN" "$HOST/foo")" "405"

step "PUT to bare /{namespace} (list endpoint) returns 400"
expect "$(http_status -X PUT -H "Authorization: Bearer $TOKEN" --data-binary @hello.txt \
  "$HOST")" "400"

# ---- max upload bytes ----
step "upload within max-upload-bytes accepted"
head -c 100000 /dev/urandom > ./under.bin
expect "$(http_status -X PUT --data-binary @under.bin -H "Authorization: Bearer $TOKEN" \
  "$HOST/under.bin")" "201"

step "upload over max-upload-bytes returns 413"
head -c 2000000 /dev/urandom > ./over.bin
expect "$(http_status -X PUT --data-binary @over.bin -H "Authorization: Bearer $TOKEN" \
  "$HOST/over.bin")" "413"

# ---- cross-namespace isolation ----
step "second namespace + token isolated from default"
out=$("$BIN" namespace create --auth-file ./auth.json other 2>&1)
OTHER_TOKEN=$(read_json_field "$out" token)
sleep 0.5
expect "$(http_status -H "Authorization: Bearer $OTHER_TOKEN" \
  "$HOST/docs/hello.txt")" "403"

# ---- public-read + hot reload ----
step "public-read off → 401 without token"
"$BIN" namespace set-public --auth-file ./auth.json --off default >/dev/null 2>&1
sleep 0.5
expect "$(http_status "$HOST/docs/hello.txt")" "401"

step "public-read on, hot-reloaded → 200 without token"
"$BIN" namespace set-public --auth-file ./auth.json --on default >/dev/null 2>&1
sleep 0.5
expect "$(http_status "$HOST/docs/hello.txt")" "200"

step "public-read off again → 401"
"$BIN" namespace set-public --auth-file ./auth.json --off default >/dev/null 2>&1
sleep 0.5
expect "$(http_status "$HOST/docs/hello.txt")" "401"

step "new namespace via CLI picked up by hot reload"
out=$("$BIN" namespace create --auth-file ./auth.json hotreload 2>&1)
HR_TOKEN=$(read_json_field "$out" token)
sleep 0.5
echo body > ./hr.txt
expect "$(http_status -X PUT --data-binary @hr.txt -H "Authorization: Bearer $HR_TOKEN" \
  "http://127.0.0.1:$PORT/hotreload/x.txt")" "201"

# ---- delete ----
step "DELETE existing object returns 200"
expect "$(http_status -X DELETE -H "Authorization: Bearer $TOKEN" \
  "$HOST/docs/hello.txt")" "200"

step "GET after DELETE returns 404"
expect "$(http_status -H "Authorization: Bearer $TOKEN" \
  "$HOST/docs/hello.txt")" "404"

# stop server cleanly before next section.
stop_server

# ---------- 7. HTTP smoke — subdomain mode (-base-domain) ----------
section "HTTP smoke — subdomain mode"

if start_server_subdomain -reload-interval 0 -max-upload-bytes 1048576; then
  info "server PID=$SERVER_PID port=$PORT base-domain=example.test"
else
  ko "failed to start server in subdomain mode"; exit 1
fi

# Same `cdn` helper, just with Host header for subdomain emulation on localhost.
HOST_SUB="http://127.0.0.1:$PORT"
HOST_HEADER='Host: default.example.test'

step "PUT via subdomain (Host header) returns 201"
echo 'sub body' > ./sub.txt
expect "$(http_status -X PUT --data-binary @sub.txt \
  -H "Authorization: Bearer $TOKEN" -H "$HOST_HEADER" \
  "$HOST_SUB/sub/file.txt")" "201"

step "GET via subdomain returns body"
body=$(curl -fsS -H "Authorization: Bearer $TOKEN" -H "$HOST_HEADER" \
  "$HOST_SUB/sub/file.txt" 2>/dev/null)
if [[ "$body" == "sub body" ]]; then ok; else ko "got: $body"; fi

step "DELETE via subdomain returns 200"
expect "$(http_status -X DELETE -H "Authorization: Bearer $TOKEN" -H "$HOST_HEADER" \
  "$HOST_SUB/sub/file.txt")" "200"

step "list at subdomain root returns objects"
echo a > ./a.txt
curl -fsS -X PUT --data-binary @a.txt \
  -H "Authorization: Bearer $TOKEN" -H "$HOST_HEADER" \
  "$HOST_SUB/list-test/a.txt" >/dev/null
n=$(curl -fsS -H "Authorization: Bearer $TOKEN" -H "$HOST_HEADER" \
  "$HOST_SUB/?prefix=/list-test" 2>/dev/null | grep -oc '"hash":"[0-9a-f]\{64\}"')
if [[ "$n" -ge 1 ]]; then ok; else ko "no entries (n=$n)"; fi

step "POST via subdomain returns 405"
expect "$(http_status -X POST -H "Authorization: Bearer $TOKEN" -H "$HOST_HEADER" \
  "$HOST_SUB/foo")" "405"

step "PUT at subdomain root (no path) returns 400"
expect "$(http_status -X PUT -H "Authorization: Bearer $TOKEN" -H "$HOST_HEADER" \
  --data-binary @a.txt "$HOST_SUB/")" "400"

step "base host (example.test) not treated as namespace"
expect "$(http_status -H "Host: example.test" -H "Authorization: Bearer $TOKEN" \
  "$HOST_SUB/docs/anything")" "404"

step "literal .. traversal via subdomain rejected (400)"
expect "$(http_status --path-as-is -H "Authorization: Bearer $TOKEN" -H "$HOST_HEADER" \
  "$HOST_SUB/docs/../etc/passwd")" "400"

step "encoded %2e%2e via subdomain rejected (400)"
expect "$(http_status --path-as-is -H "Authorization: Bearer $TOKEN" -H "$HOST_HEADER" \
  "$HOST_SUB/docs/%2e%2e/etc/passwd")" "400"

stop_server

# ---------- 8. backup / restore / GC ----------
section "backup / restore / GC"

step "GC default 1h grace skips recent files"
out=$("$BIN" gc --data-dir ./data 2>&1)
if echo "$out" | grep -q '"deleted_blobs": 0'; then ok; else ko "$out"; fi

step "GC --grace 0s deletes orphan blob from delete"
out=$("$BIN" gc --data-dir ./data --grace 0s 2>&1)
if echo "$out" | grep -qE '"deleted_blobs": [1-9]'; then ok
else ko "expected deletions; got: $out"; fi

step "backup writes a tarball with auth.json and data/aliases"
"$BIN" backup --data-dir ./data --auth-file ./auth.json --out ./backup.tgz 2>"$WORK/backup.err"
tar_listing=$(tar -tzf ./backup.tgz 2>/dev/null)
if grep -qE '^auth\.json$' <<<"$tar_listing" && \
   grep -qE '^data/aliases/' <<<"$tar_listing"; then
  ok
else
  ko "backup missing expected entries (see $WORK/backup.err)"
fi

step "restore into empty target succeeds"
mkdir -p ./restored/data
out=$("$BIN" restore --data-dir ./restored/data --auth-file ./restored/auth.json --in ./backup.tgz 2>&1)
if echo "$out" | grep -q '"status": "restored"'; then ok; else ko "$out"; fi

step "restored auth.json has mode 0600"
mode=$(stat -c '%a' ./restored/auth.json 2>/dev/null)
expect "$mode" "600"

step "restored namespace list matches"
n=$("$BIN" namespace list --auth-file ./restored/auth.json 2>&1 | grep -c '"name":')
if [[ "$n" -ge 2 ]]; then ok; else ko "got $n namespaces in restore"; fi

step "restore into non-empty target without --force is refused"
if "$BIN" restore --data-dir ./restored/data --auth-file ./restored/auth.json --in ./backup.tgz >/dev/null 2>&1; then
  ko "expected refusal"
else
  ok
fi

# ---------- 9. restart server for load tests ----------
section "restart server for load tests"

if start_server -reload-interval 0 -max-upload-bytes 1073741824; then
  info "server PID=$SERVER_PID port=$PORT no-reload max-upload=1GiB base-domain=<none>"
else
  ko "failed to restart server"
fi

HOST="http://127.0.0.1:$PORT/default"

# Seed objects for load runs.
echo load-body > ./load.txt
dd if=/dev/urandom of=./med.bin bs=1024 count=64 status=none 2>/dev/null
curl -sf -X PUT --data-binary @load.txt -H "Authorization: Bearer $TOKEN" \
  "$HOST/load.txt" >/dev/null 2>&1 || true
curl -sf -X PUT --data-binary @med.bin -H "Authorization: Bearer $TOKEN" \
  "$HOST/med.bin" >/dev/null 2>&1 || true

# ---------- 10. load tests ----------
if [[ "$SKIP_LOAD" == "0" ]]; then
  section "load tests (optional)"

  BOMB=$(find_tool bombardier)
  if [[ -z "$BOMB" ]]; then
    info "bombardier not found (install: GOBIN=~/.local/bin go install github.com/codesenberg/bombardier@latest)"
  else
    step "bombardier /healthz 3s c=100"
    if "$BOMB" --http1 -d 3s -c 100 -p result \
        "http://127.0.0.1:$PORT/healthz" >"$WORK/bomb-healthz.log" 2>&1; then
      ok
      grep -E 'Reqs/sec|Latency|HTTP codes' "$WORK/bomb-healthz.log" | head -6 | sed 's/^/       /'
    else
      ko "see $WORK/bomb-healthz.log"
    fi

    step "bombardier GET load.txt path-fallback 3s c=100"
    if "$BOMB" --http1 -d 3s -c 100 -p result \
        -H "Authorization: Bearer $TOKEN" \
        "$HOST/load.txt" >"$WORK/bomb-get.log" 2>&1; then
      ok
      grep -E 'Reqs/sec|Latency|HTTP codes' "$WORK/bomb-get.log" | head -6 | sed 's/^/       /'
    else
      ko "see $WORK/bomb-get.log"
    fi

    step "bombardier GET med.bin (64KB) 3s c=64"
    if "$BOMB" --http1 -d 3s -c 64 -p result \
        -H "Authorization: Bearer $TOKEN" \
        "$HOST/med.bin" >"$WORK/bomb-med.log" 2>&1; then
      ok
      grep -E 'Reqs/sec|Latency|HTTP codes|Throughput' "$WORK/bomb-med.log" | head -6 | sed 's/^/       /'
    else
      ko "see $WORK/bomb-med.log"
    fi
  fi

  VEG=$(find_tool vegeta)
  if [[ -z "$VEG" ]]; then
    info "vegeta not found (install: GOBIN=~/.local/bin go install github.com/tsenart/vegeta/v12@latest)"
  else
    step "vegeta GET ramp 1000/s for 3s"
    printf 'GET %s/load.txt\nAuthorization: Bearer %s\n' "$HOST" "$TOKEN" > "$WORK/veg.targets"
    if "$VEG" attack -duration=3s -rate=1000/s -connections=64 -keepalive \
        -targets="$WORK/veg.targets" -output="$WORK/veg.bin" >/dev/null 2>&1; then
      ok
      "$VEG" report -type=text "$WORK/veg.bin" \
        | grep -E 'Requests|Latencies|Success|Status' | sed 's/^/       /'
    else
      ko "vegeta attack failed"
    fi
  fi
fi

# ---------- 11. summary ----------
section "summary"

if [[ "$FAIL" -eq 0 ]]; then
  printf "${GREEN}${BOLD}ALL %d CHECKS PASSED${NC}\n" "$STEP_NUM"
  EXIT_CODE=0
else
  printf "${RED}${BOLD}%d / %d CHECKS FAILED${NC}\n" "$FAIL" "$STEP_NUM"
  EXIT_CODE=1
fi

# Make sure the EXIT trap returns the right code.
trap - EXIT
cleanup_rc() {
  stop_server
  if [[ "$KEEP_WORK" == "1" ]]; then
    echo "${YELLOW}keeping work dir:${NC} $WORK"
  else
    rm -rf "$WORK"
  fi
}
cleanup_rc
exit "$EXIT_CODE"
