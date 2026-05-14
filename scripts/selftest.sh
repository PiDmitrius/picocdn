#!/usr/bin/env bash
# picocdn end-to-end self-test.
#
# Runs: gofmt, go vet, go test, go test -race, microbenchmarks, build,
# CLI smoke (init/root), HTTP smoke via path-fallback (no DNS required),
# admin plane checks, subdomain-mode section, GC/backup/restore, and
# optional load tests (bombardier, vegeta) if those tools exist.
#
# The test isolates state under a temp HOME so it never touches the real
# ~/.config/picocdn or ~/.local/share/picocdn.

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
DATA="$WORK/data"
SAVED_HOME="$HOME"

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

expect() {
  if [[ "$1" == "$2" ]]; then ok; else ko "want=$2 got=$1 ${3:-}"; fi
}

http_status() {
  curl -s -o /dev/null -w '%{http_code}' --max-time 10 "$@" 2>/dev/null || echo "curl_failed"
}

# read_json_field <json> <field>: extract first quoted string value for a
# field, handling JSON on a single line or across lines.
read_json_field() {
  echo "$1" | tr -d '\n' | grep -oE '"'"$2"'"[[:space:]]*:[[:space:]]*"[^"]*"' \
    | head -1 \
    | sed -E 's/^"[^"]+"[[:space:]]*:[[:space:]]*"(.*)"$/\1/'
}

# json_post|delete: helpers for admin plane operations.
admin_call() {
  local method="$1" url="$2" token="$3" body="${4:-}"
  if [[ -n "$body" ]]; then
    curl -fsS -X "$method" \
      -H "Authorization: Bearer $token" \
      -H "Content-Type: application/json" \
      --data "$body" \
      "$url"
  else
    curl -fsS -X "$method" \
      -H "Authorization: Bearer $token" \
      "$url"
  fi
}

start_server() { _start_server "" "$@"; }
start_server_subdomain() { _start_server "example.test" "$@"; }

_start_server() {
  local base="$1"; shift
  local -a flags=( -addr "127.0.0.1:$PORT" -data-dir "$DATA" )
  if [[ -n "$base" ]]; then
    flags+=( -base-domain "$base" )
  fi
  flags+=( "$@" )
  cd "$WORK"
  ( HOME="$WORK" "$BIN" serve "${flags[@]}" >./srv.out 2>./srv.err ) &
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

find_tool() {
  local name="$1"
  if command -v "$name" >/dev/null 2>&1; then
    command -v "$name"
  elif [[ -x "$HOME/.local/bin/$name" ]]; then
    echo "$HOME/.local/bin/$name"
  elif [[ -x "$SAVED_HOME/.local/bin/$name" ]]; then
    echo "$SAVED_HOME/.local/bin/$name"
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

mkdir -p "$DATA"

# From here on, HOME is the work dir so config.json lives inside $WORK.
export HOME="$WORK"

# ---------- 5. CLI smoke (init / root) ----------
section "CLI smoke (init / root)"

step "picocdn init creates first root token"
out=$("$BIN" init 2>&1)
ROOT_TOKEN=$(read_json_field "$out" token)
if [[ -n "$ROOT_TOKEN" && "$ROOT_TOKEN" == prt_* ]]; then ok
else ko "no plaintext root token in: $out"; fi

step "config.json has mode 0600"
mode=$(stat -c '%a' "$WORK/.config/picocdn/config.json" 2>/dev/null)
expect "$mode" "600"

step "config.json contains no plaintext root token"
if grep -qF "$ROOT_TOKEN" "$WORK/.config/picocdn/config.json" 2>/dev/null; then
  ko "plaintext token leaked into config.json"
else
  ok
fi

step "picocdn init refuses to re-bootstrap"
if "$BIN" init >/dev/null 2>&1; then ko "second init should fail"; else ok; fi

step "picocdn root create issues second root"
out=$("$BIN" root create --name extra 2>&1)
EXTRA_ROOT=$(read_json_field "$out" token)
if [[ -n "$EXTRA_ROOT" && "$EXTRA_ROOT" == prt_* ]]; then ok
else ko "no extra root: $out"; fi

step "picocdn root list shows 2 entries"
n=$("$BIN" root list 2>&1 | grep -c '"id":')
expect "$n" "2"

step "picocdn root revoke removes second root"
EXTRA_ID=$(echo "$out" | awk -F\" '/"token_id":/ {print $4; exit}')
if "$BIN" root revoke "$EXTRA_ID" >/dev/null 2>&1; then ok
else ko "revoke failed"; fi

step "picocdn root revoke refuses the last root"
LAST_ID=$("$BIN" root list 2>&1 | awk -F\" '/"id":/ {print $4; exit}')
if "$BIN" root revoke "$LAST_ID" >/dev/null 2>&1; then
  ko "revoke of last root should fail"
else
  ok
fi

step "picocdn version prints version"
out=$("$BIN" version 2>&1)
if [[ "$out" == picocdn* ]]; then ok; else ko "$out"; fi

# ---------- 6. HTTP smoke (admin + objects, path-fallback) ----------
section "HTTP smoke — admin and objects (path-fallback)"

if start_server -max-upload-bytes 1048576; then
  info "server PID=$SERVER_PID port=$PORT max-upload=1MB base-domain=<none>"
else
  ko "failed to start server"; exit 1
fi

ADMIN="http://127.0.0.1:$PORT/_/namespaces"
BASE="http://127.0.0.1:$PORT"

step "GET /healthz returns 200"
expect "$(http_status "$BASE/healthz")" "200"

step "POST /_/namespaces creates default with owner"
out=$(admin_call POST "$ADMIN" "$ROOT_TOKEN" '{"name":"default"}' 2>&1)
OWNER_TOKEN=$(read_json_field "$out" owner_token)
if [[ -n "$OWNER_TOKEN" && "$OWNER_TOKEN" == pcd_* ]]; then ok
else ko "no owner_token in: $out"; fi

step "namespaces/default.json has mode 0600"
mode=$(stat -c '%a' "$DATA/namespaces/default.json" 2>/dev/null)
expect "$mode" "600"

step "namespaces/default.json contains no plaintext token"
if grep -qF "$OWNER_TOKEN" "$DATA/namespaces/default.json" 2>/dev/null; then
  ko "plaintext token leaked"
else
  ok
fi

step "POST duplicate namespace returns 409"
expect "$(http_status -X POST -H "Authorization: Bearer $ROOT_TOKEN" \
  -H "Content-Type: application/json" \
  --data '{"name":"default"}' "$ADMIN")" "409"

step "POST namespace with leading underscore returns 400"
expect "$(http_status -X POST -H "Authorization: Bearer $ROOT_TOKEN" \
  -H "Content-Type: application/json" \
  --data '{"name":"_evil"}' "$ADMIN")" "400"

step "POST namespace without root token returns 403"
expect "$(http_status -X POST -H "Authorization: Bearer $OWNER_TOKEN" \
  -H "Content-Type: application/json" \
  --data '{"name":"x"}' "$ADMIN")" "403"

step "GET /_/namespaces lists default"
out=$(admin_call GET "$ADMIN" "$ROOT_TOKEN" 2>&1)
if echo "$out" | grep -q '"default"'; then ok; else ko "$out"; fi

step "POST /_/namespaces/default/tokens issues read+write token"
out=$(admin_call POST "$ADMIN/default/tokens" "$OWNER_TOKEN" '{"name":"ci","permissions":["read","write"]}' 2>&1)
RW_TOKEN=$(read_json_field "$out" token)
if [[ -n "$RW_TOKEN" && "$RW_TOKEN" == pcd_* ]]; then ok; else ko "$out"; fi

step "POST tokens with owner permission rejected (400)"
expect "$(http_status -X POST -H "Authorization: Bearer $OWNER_TOKEN" \
  -H "Content-Type: application/json" \
  --data '{"name":"evil","permissions":["owner"]}' \
  "$ADMIN/default/tokens")" "400"

step "POST tokens by sub-token rejected (401, no admin scope)"
expect "$(http_status -X POST -H "Authorization: Bearer $RW_TOKEN" \
  -H "Content-Type: application/json" \
  --data '{"name":"x","permissions":["read"]}' \
  "$ADMIN/default/tokens")" "401"

step "GET /_/namespaces/default/tokens lists 2 tokens"
out=$(admin_call GET "$ADMIN/default/tokens" "$OWNER_TOKEN" 2>&1)
n=$(echo "$out" | tr -d '\n' | grep -oE '"id"[[:space:]]*:' | wc -l)
expect "$n" "2"

# ---- object operations ----
cdn() { curl -fsS -H "Authorization: Bearer $OWNER_TOKEN" "$@"; }
HOST="$BASE/default"

echo 'hello picocdn' > "$WORK/hello.txt"
step "PUT object as owner returns 201"
up=$(cdn -T "$WORK/hello.txt" -H 'Content-Type: text/plain' "$HOST/docs/hello.txt" 2>/dev/null) || up=""
HASH=$(echo "$up" | grep -oE '"hash":"[0-9a-f]{64}"' | head -1 | tr -d '"' | sed 's/hash://')
if [[ -n "$HASH" ]]; then ok; else ko "no hash; body: $up"; fi

step "GET object returns body"
body=$(cdn "$HOST/docs/hello.txt" 2>/dev/null)
if [[ "$body" == "hello picocdn" ]]; then ok; else ko "got: $body"; fi

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

step "If-None-Match returns 304"
expect "$(http_status -H "Authorization: Bearer $OWNER_TOKEN" \
  -H "If-None-Match: \"sha256:$HASH\"" \
  "$HOST/docs/hello.txt")" "304"

step "list at /{namespace} returns objects"
n=$(cdn "$HOST" 2>/dev/null | grep -oc '"hash":"[0-9a-f]\{64\}"')
if [[ "$n" -ge 1 ]]; then ok; else ko "no entries (n=$n)"; fi

step "list with ?prefix= narrows"
n=$(cdn "$HOST?prefix=/docs" 2>/dev/null | grep -oc '"hash":"[0-9a-f]\{64\}"')
if [[ "$n" -ge 1 ]]; then ok; else ko "no entries (n=$n)"; fi

# ---- auth checks ----
step "missing token on GET returns 401"
expect "$(http_status "$HOST/docs/hello.txt")" "401"

step "wrong token returns 401 (unified, no existence leak)"
expect "$(http_status -H "Authorization: Bearer wrong" "$HOST/docs/hello.txt")" "401"

step "read+write token CAN write (201)"
echo body > "$WORK/x.txt"
expect "$(http_status -X PUT --data-binary @"$WORK/x.txt" \
  -H "Authorization: Bearer $RW_TOKEN" "$HOST/x.txt")" "201"

step "read+write token CANNOT delete (403)"
expect "$(http_status -X DELETE -H "Authorization: Bearer $RW_TOKEN" \
  "$HOST/x.txt")" "403"

step "root token works on object plane (201)"
echo from-root > "$WORK/r.txt"
expect "$(http_status -X PUT --data-binary @"$WORK/r.txt" \
  -H "Authorization: Bearer $ROOT_TOKEN" "$HOST/root.txt")" "201"

step "PUT to nonexistent namespace via owner token returns 401 (no leak)"
expect "$(http_status -X PUT --data-binary @"$WORK/x.txt" \
  -H "Authorization: Bearer $OWNER_TOKEN" \
  "$BASE/missing/x.txt")" "401"

step "root sees 404 for missing namespace (root knows the list)"
expect "$(http_status -X PUT --data-binary @"$WORK/x.txt" \
  -H "Authorization: Bearer $ROOT_TOKEN" \
  "$BASE/missing/x.txt")" "404"

step "encoded %2e%2e traversal rejected (400)"
expect "$(http_status --path-as-is -H "Authorization: Bearer $OWNER_TOKEN" \
  "$HOST/docs/%2e%2e/etc/passwd")" "400"

step "POST on object path returns 405"
expect "$(http_status -X POST -H "Authorization: Bearer $OWNER_TOKEN" "$HOST/foo")" "405"

step "PUT to bare /{namespace} (list endpoint) returns 400"
expect "$(http_status -X PUT -H "Authorization: Bearer $OWNER_TOKEN" --data-binary @"$WORK/x.txt" \
  "$HOST")" "400"

step "GET /_ returns 404 (admin discovery disabled)"
expect "$(http_status "$BASE/_")" "404"

step "GET /_/ returns 404"
expect "$(http_status "$BASE/_/")" "404"

# ---- max upload bytes ----
step "upload within max-upload-bytes accepted"
head -c 100000 /dev/urandom > "$WORK/under.bin"
expect "$(http_status -X PUT --data-binary @"$WORK/under.bin" \
  -H "Authorization: Bearer $OWNER_TOKEN" \
  "$HOST/under.bin")" "201"

step "upload over max-upload-bytes returns 413"
head -c 2000000 /dev/urandom > "$WORK/over.bin"
expect "$(http_status -X PUT --data-binary @"$WORK/over.bin" \
  -H "Authorization: Bearer $OWNER_TOKEN" \
  "$HOST/over.bin")" "413"

# ---- cross-namespace isolation ----
step "create second namespace via admin"
out=$(admin_call POST "$ADMIN" "$ROOT_TOKEN" '{"name":"other"}' 2>&1)
OTHER_OWNER=$(read_json_field "$out" owner_token)
if [[ -n "$OTHER_OWNER" ]]; then ok; else ko "$out"; fi

step "cross-namespace token returns 401 (no existence leak)"
expect "$(http_status -H "Authorization: Bearer $OTHER_OWNER" \
  "$HOST/docs/hello.txt")" "401"

step "rotate-owner invalidates old owner (401)"
new_out=$(admin_call POST "$ADMIN/default/rotate-owner" "$ROOT_TOKEN" 2>&1)
NEW_OWNER=$(read_json_field "$new_out" owner_token)
if [[ -n "$NEW_OWNER" ]]; then
  expect "$(http_status -X PUT --data-binary @"$WORK/x.txt" \
    -H "Authorization: Bearer $OWNER_TOKEN" "$HOST/after-rotate.txt")" "401"
else
  ko "no new owner_token in rotate response"
fi
OWNER_TOKEN="$NEW_OWNER"

step "new owner token works after rotate (201)"
expect "$(http_status -X PUT --data-binary @"$WORK/x.txt" \
  -H "Authorization: Bearer $OWNER_TOKEN" "$HOST/after-rotate.txt")" "201"

# ---- public-read via admin ----
step "public-read off → 401 without token"
admin_call POST "$ADMIN/default/public" "$OWNER_TOKEN" '{"on":false}' >/dev/null
expect "$(http_status "$HOST/docs/hello.txt")" "401"

step "public-read on → 200 without token"
admin_call POST "$ADMIN/default/public" "$OWNER_TOKEN" '{"on":true}' >/dev/null
expect "$(http_status "$HOST/docs/hello.txt")" "200"

step "public-read on still requires token for listing"
expect "$(http_status "$HOST")" "401"

step "public-read off again → 401"
admin_call POST "$ADMIN/default/public" "$OWNER_TOKEN" '{"on":false}' >/dev/null
expect "$(http_status "$HOST/docs/hello.txt")" "401"

# ---- delete object ----
step "DELETE existing object returns 200"
expect "$(http_status -X DELETE -H "Authorization: Bearer $OWNER_TOKEN" \
  "$HOST/docs/hello.txt")" "200"

step "GET after DELETE returns 404"
expect "$(http_status -H "Authorization: Bearer $OWNER_TOKEN" \
  "$HOST/docs/hello.txt")" "404"

# ---- revoke sub-token ----
step "issue fresh sub-token for revoke test"
out=$(admin_call POST "$ADMIN/default/tokens" "$OWNER_TOKEN" '{"name":"doomed","permissions":["read"]}' 2>&1)
DOOMED_ID=$(read_json_field "$out" token_id)
if [[ -n "$DOOMED_ID" ]]; then ok; else ko "$out"; fi

step "DELETE sub-token via admin returns 204"
expect "$(http_status -X DELETE -H "Authorization: Bearer $OWNER_TOKEN" \
  "$ADMIN/default/tokens/$DOOMED_ID")" "204"

step "DELETE owner token via admin refused (400)"
# Find owner token id: the one with "owner": true.
OID=$(admin_call GET "$ADMIN/default/tokens" "$OWNER_TOKEN" 2>&1 \
  | tr -d '\n' \
  | grep -oE '\{[^{}]*"owner"[[:space:]]*:[[:space:]]*true[^{}]*\}' \
  | head -1 \
  | grep -oE '"id"[[:space:]]*:[[:space:]]*"[^"]+"' \
  | head -1 \
  | sed -E 's/.*"([^"]+)"$/\1/')
expect "$(http_status -X DELETE -H "Authorization: Bearer $OWNER_TOKEN" \
  "$ADMIN/default/tokens/$OID")" "400"

# ---- delete whole namespace ----
step "DELETE /_/namespaces/other returns 204"
expect "$(http_status -X DELETE -H "Authorization: Bearer $ROOT_TOKEN" \
  "$ADMIN/other")" "204"

step "deleted namespace no longer accepts PUT (404)"
expect "$(http_status -X PUT --data-binary @"$WORK/x.txt" \
  -H "Authorization: Bearer $ROOT_TOKEN" \
  "$BASE/other/y.txt")" "404"

stop_server

# ---------- 7. HTTP smoke — subdomain mode ----------
section "HTTP smoke — subdomain mode"

if start_server_subdomain -max-upload-bytes 1048576; then
  info "server PID=$SERVER_PID port=$PORT base-domain=example.test"
else
  ko "failed to start server in subdomain mode"; exit 1
fi

HOST_HEADER='Host: default.example.test'

step "PUT via subdomain (Host header) returns 201"
echo 'sub body' > "$WORK/sub.txt"
expect "$(http_status -X PUT --data-binary @"$WORK/sub.txt" \
  -H "Authorization: Bearer $OWNER_TOKEN" -H "$HOST_HEADER" \
  "$BASE/sub/file.txt")" "201"

step "GET via subdomain returns body"
body=$(curl -fsS -H "Authorization: Bearer $OWNER_TOKEN" -H "$HOST_HEADER" \
  "$BASE/sub/file.txt" 2>/dev/null)
if [[ "$body" == "sub body" ]]; then ok; else ko "got: $body"; fi

step "DELETE via subdomain returns 200"
expect "$(http_status -X DELETE -H "Authorization: Bearer $OWNER_TOKEN" -H "$HOST_HEADER" \
  "$BASE/sub/file.txt")" "200"

step "POST via subdomain returns 405"
expect "$(http_status -X POST -H "Authorization: Bearer $OWNER_TOKEN" -H "$HOST_HEADER" \
  "$BASE/foo")" "405"

step "PUT at subdomain root (no path) returns 400"
expect "$(http_status -X PUT -H "Authorization: Bearer $OWNER_TOKEN" -H "$HOST_HEADER" \
  --data-binary @"$WORK/sub.txt" "$BASE/")" "400"

step "base host (example.test) falls back to path routing"
# falls into path-fallback /docs/anything; "docs" namespace does not exist
# and token doesn't match it — unified 401 instead of leaking 404.
expect "$(http_status -H "Host: example.test" -H "Authorization: Bearer $OWNER_TOKEN" \
  "$BASE/docs/anything")" "401"

step "underscore-prefix subdomain not routed; falls back to path"
expect "$(http_status -H "Host: _bad.example.test" -H "Authorization: Bearer $OWNER_TOKEN" \
  "$BASE/x")" "401"

step "encoded %2e%2e via subdomain rejected (400)"
expect "$(http_status --path-as-is -H "Authorization: Bearer $OWNER_TOKEN" -H "$HOST_HEADER" \
  "$BASE/docs/%2e%2e/etc/passwd")" "400"

stop_server

# ---------- 8. fail-fast on bad namespace file ----------
section "fail-fast loader"

step "broken namespaces/<x>.json prevents server start"
echo 'not json' > "$DATA/namespaces/broken.json"
if HOME="$WORK" "$BIN" serve -addr "127.0.0.1:$PORT" -data-dir "$DATA" >"$WORK/badstart.out" 2>"$WORK/badstart.err"; then
  ko "server should have failed to start"
else
  if grep -q "load auth" "$WORK/badstart.err"; then ok
  else ko "wrong error: $(cat "$WORK/badstart.err")"; fi
fi
rm -f "$DATA/namespaces/broken.json"

# ---------- 9. backup / restore / GC ----------
section "backup / restore / GC"

step "GC default 1h grace skips recent files"
out=$("$BIN" gc --data-dir "$DATA" 2>&1)
if echo "$out" | grep -q '"deleted_blobs": 0'; then ok; else ko "$out"; fi

step "GC --grace 0s deletes orphan blob from delete"
out=$("$BIN" gc --data-dir "$DATA" --grace 0s 2>&1)
if echo "$out" | grep -qE '"deleted_blobs": [1-9]'; then ok
else ko "expected deletions; got: $out"; fi

step "backup writes a tarball with config and data/namespaces"
"$BIN" backup --data-dir "$DATA" --out "$WORK/backup.tgz" 2>"$WORK/backup.err"
tar_listing=$(tar -tzf "$WORK/backup.tgz" 2>/dev/null)
if grep -qE '^config\.json$' <<<"$tar_listing" && \
   grep -qE '^data/namespaces/' <<<"$tar_listing" && \
   grep -qE '^data/aliases/' <<<"$tar_listing"; then
  ok
else
  ko "backup missing expected entries (see $WORK/backup.err)"
fi

step "restore into empty target succeeds"
RESTORE="$WORK/restored"
mkdir -p "$RESTORE/data"
mkdir -p "$RESTORE/.config/picocdn"
out=$(HOME="$RESTORE" "$BIN" restore --data-dir "$RESTORE/data" --in "$WORK/backup.tgz" 2>&1)
if echo "$out" | grep -q '"status": "restored"'; then ok; else ko "$out"; fi

step "restored config.json has mode 0600"
mode=$(stat -c '%a' "$RESTORE/.config/picocdn/config.json" 2>/dev/null)
expect "$mode" "600"

step "restored namespaces have mode 0600"
mode=$(stat -c '%a' "$RESTORE/data/namespaces/default.json" 2>/dev/null)
expect "$mode" "600"

step "restored server starts and lists default"
HOME="$RESTORE" "$BIN" serve -addr "127.0.0.1:$PORT" -data-dir "$RESTORE/data" >"$WORK/restart.out" 2>"$WORK/restart.err" &
RESTORED_PID=$!
for _ in 1 2 3 4 5 6 7 8 9 10; do
  if curl -fsS --max-time 1 "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1; then
    break
  fi
  sleep 0.1
done
out=$(curl -fsS -H "Authorization: Bearer $ROOT_TOKEN" "$BASE/_/namespaces" 2>&1)
if echo "$out" | grep -q '"default"'; then ok; else ko "$out"; fi
kill "$RESTORED_PID" 2>/dev/null || true
wait "$RESTORED_PID" 2>/dev/null || true

step "restore into non-empty target without --force is refused"
if HOME="$RESTORE" "$BIN" restore --data-dir "$RESTORE/data" --in "$WORK/backup.tgz" >/dev/null 2>&1; then
  ko "expected refusal"
else
  ok
fi

# ---------- 10. restart for load tests ----------
section "restart server for load tests"

if start_server -max-upload-bytes 1073741824; then
  info "server PID=$SERVER_PID port=$PORT max-upload=1GiB base-domain=<none>"
else
  ko "failed to restart server"
fi

HOST="$BASE/default"

# Seed objects for load runs.
echo load-body > "$WORK/load.txt"
dd if=/dev/urandom of="$WORK/med.bin" bs=1024 count=64 status=none 2>/dev/null
curl -sf -X PUT --data-binary @"$WORK/load.txt" -H "Authorization: Bearer $OWNER_TOKEN" \
  "$HOST/load.txt" >/dev/null 2>&1 || true
curl -sf -X PUT --data-binary @"$WORK/med.bin" -H "Authorization: Bearer $OWNER_TOKEN" \
  "$HOST/med.bin" >/dev/null 2>&1 || true

# ---------- 11. load tests ----------
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

    step "bombardier GET load.txt 3s c=100"
    if "$BOMB" --http1 -d 3s -c 100 -p result \
        -H "Authorization: Bearer $OWNER_TOKEN" \
        "$HOST/load.txt" >"$WORK/bomb-get.log" 2>&1; then
      ok
      grep -E 'Reqs/sec|Latency|HTTP codes' "$WORK/bomb-get.log" | head -6 | sed 's/^/       /'
    else
      ko "see $WORK/bomb-get.log"
    fi

    step "bombardier GET med.bin (64KB) 3s c=64"
    if "$BOMB" --http1 -d 3s -c 64 -p result \
        -H "Authorization: Bearer $OWNER_TOKEN" \
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
    printf 'GET %s/load.txt\nAuthorization: Bearer %s\n' "$HOST" "$OWNER_TOKEN" > "$WORK/veg.targets"
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

# ---------- 12. summary ----------
section "summary"

if [[ "$FAIL" -eq 0 ]]; then
  printf "${GREEN}${BOLD}ALL %d CHECKS PASSED${NC}\n" "$STEP_NUM"
  EXIT_CODE=0
else
  printf "${RED}${BOLD}%d / %d CHECKS FAILED${NC}\n" "$FAIL" "$STEP_NUM"
  EXIT_CODE=1
fi

trap - EXIT
stop_server
if [[ "$KEEP_WORK" == "1" ]]; then
  echo "${YELLOW}keeping work dir:${NC} $WORK"
else
  rm -rf "$WORK"
fi
exit "$EXIT_CODE"
