# picocdn

Small self-hosted CDN/storage service for AI-generated artifacts. Go stdlib only.

## Quick start

```sh
# Create namespace and owner token (prints plaintext token once).
picocdn namespace create --auth-file ./auth.json default

# Run the server — works out of the box on http://127.0.0.1:8080/{namespace}/...
picocdn serve -addr 127.0.0.1:8080 -data-dir ./data -auth-file ./auth.json
```

Subdomain routing (`https://{namespace}.your-domain.tld/...`) is **opt-in** —
turn it on by passing `-base-domain your-domain.tld` and putting Caddy with
wildcard DNS in front. Out of the box you address objects through the path
under the listen port; both styles share the same verbs and tokens.

Configuration via env or flags:

| env                          | flag                | default              |
|------------------------------|---------------------|----------------------|
| `PICOCDN_ADDR`               | `-addr`             | `:8080`              |
| `PICOCDN_DATA_DIR`           | `-data-dir`         | `/var/lib/picocdn`   |
| `PICOCDN_AUTH_FILE`          | `-auth-file`        | `$DATA_DIR/auth.json`|
| `PICOCDN_BASE_DOMAIN`        | `-base-domain`      | _empty_ (subdomain routing off) |
| `PICOCDN_MAX_UPLOAD_BYTES`   | `-max-upload-bytes` | `1073741824`         |
| `PICOCDN_RELOAD_INTERVAL`    | `-reload-interval`  | `5s` (0 disables)    |

## API

One endpoint shape, four verbs, namespace as the first path segment (or as a
subdomain when you opt in). Examples assume you reach the server at
`http://127.0.0.1:8080`; for a TLS deployment swap that for your Caddy-fronted
URL.

```
PUT    http://host/{ns}/foo/bar.bin       upload (body = file, Content-Type from header)
GET    http://host/{ns}/foo/bar.bin       download
HEAD   http://host/{ns}/foo/bar.bin       metadata only
DELETE http://host/{ns}/foo/bar.bin       delete
GET    http://host/{ns}[?prefix=/x]       list objects under namespace

GET    /healthz                            liveness (auth-free)
```

### Subdomain routing (optional)

Run `picocdn serve -base-domain your-domain.tld` and the same handlers also
respond to `https://{ns}.your-domain.tld/...`. The namespace then comes from
the subdomain instead of the first URL segment; the verbs and the auth model
are unchanged.

```caddyfile
your-domain.tld, *.your-domain.tld {
    reverse_proxy 127.0.0.1:8080
}
```

The base host (no subdomain) never resolves to a namespace.

## curl recipes

One-time setup in your shell — defines `$HOST` and a tiny `cdn` wrapper so
every recipe is a single short line:

```sh
TOKEN=pcd_xxxxxxxxxxxxxxxxxxxxxxxxxxxx

# Default: path-fallback, no TLS, no DNS — works against `picocdn serve` directly.
HOST=http://127.0.0.1:8080/default

# Or, if you enabled subdomain routing with -base-domain:
# HOST=https://default.your-domain.tld

cdn() { curl -fsS -H "Authorization: Bearer $TOKEN" "$@"; }
```

Both forms work with every recipe below — only `$HOST` changes.

### Upload

```sh
# one file
cdn -T cat.png $HOST/docs/cat.png

# explicit content-type
cdn -T blob -H 'Content-Type: application/x-protobuf' $HOST/raw/blob

# big file with progress bar
cdn --progress-bar -T dump.tar.zst $HOST/dumps/dump.tar.zst

# from stdin (no temp file on disk)
tar c src/ | cdn -T - $HOST/snapshots/src.tar
```

Folder recursive:

```sh
( cd src && find . -type f -print0 | while IFS= read -r -d '' f; do
    cdn -T "$f" "$HOST/src/${f#./}" >/dev/null && echo "ok  /src/${f#./}"
  done )
```

Folder parallel (`curl -Z`, one process, many connections):

```sh
ARGS=()
while IFS= read -r -d '' f; do
  ARGS+=( --next -T "$f" -H "Authorization: Bearer $TOKEN" "$HOST/src/${f#./}" )
done < <(cd src && find . -type f -print0)
curl -Z -fsS -o /dev/null "${ARGS[@]:1}"
```

### Download

```sh
# whole file
cdn -o cat.png $HOST/docs/cat.png

# byte range
cdn --range 0-1023 -o head.bin $HOST/big.bin

# resume after a broken transfer (uses Range: bytes=<size>-)
cdn --continue-at - -o dump.tar.zst $HOST/dumps/dump.tar.zst

# metadata only
cdn -I $HOST/docs/cat.png

# conditional GET — 304 if ETag matches
ETAG=$(cdn -I $HOST/docs/cat.png | awk -F': ' 'tolower($1)=="etag"{print $2}' | tr -d '\r')
cdn -i -H "If-None-Match: $ETAG" $HOST/docs/cat.png | head -1
```

### Delete

```sh
cdn -X DELETE $HOST/docs/cat.png
```

### List

```sh
cdn "$HOST/?prefix=/docs" | jq
```

Pull a whole prefix into a local tree:

```sh
cdn "$HOST/?prefix=/src" | jq -r '.objects[].path' \
  | while read -r p; do
      mkdir -p "out$(dirname "$p")"
      cdn -o "out$p" "$HOST$p"
    done
```

## Public-read namespaces

```sh
picocdn namespace set-public --auth-file ./auth.json --on  default
picocdn namespace set-public --auth-file ./auth.json --off default
```

When public-read is enabled, `GET`/`HEAD` work without a token. `PUT` and
`DELETE` still require a token with the appropriate permission. Cache-Control
is relaxed to `public, max-age=300` for public namespaces.

## Tokens

```sh
picocdn token create --auth-file ./auth.json --name uploader --perm read --perm write default
picocdn token list   --auth-file ./auth.json default
picocdn token revoke --auth-file ./auth.json default <token_id>
```

Permissions: `read`, `write`, `delete`, `admin`, `owner`. `auth.json` stores
only SHA-256 token hashes; the plaintext token is printed once.

`auth.json` is **hot-reloaded** every `PICOCDN_RELOAD_INTERVAL` (default 5s).
Replacing a non-empty auth file with an empty one is refused so an accidental
write does not silently lock everyone out. A deleted file is treated the same:
the previous good state is kept and a warning is logged.

## Namespace management

```sh
picocdn namespace create  --auth-file ./auth.json <name>
picocdn namespace list    --auth-file ./auth.json
picocdn namespace show    --auth-file ./auth.json <name>
picocdn namespace delete  --auth-file ./auth.json --force <name>
picocdn namespace set-public --auth-file ./auth.json --on|--off <name>
```

Namespaces must be lowercase DNS labels (`a-z`, `0-9`, `-`, 1–63 chars, no
leading or trailing hyphen) so they remain valid as a subdomain label if
subdomain routing is enabled later.

`namespace delete` only removes the auth record. The data directory is left
alone; run `gc` afterwards if you want the blobs reclaimed.

## Backup, restore, GC

```sh
picocdn backup  --data-dir ./data --auth-file ./auth.json --out ./pico.tgz
picocdn restore --data-dir ./data --auth-file ./auth.json --in  ./pico.tgz [--force]
picocdn gc      --data-dir ./data [--grace 1h]
```

- The backup tarball includes `auth.json` plus `data/blobs/` and `data/aliases/`.
- `restore` refuses to overwrite a non-empty target unless `--force` is given.
- `gc` walks every alias, then deletes any blob that no alias references.
  Files newer than `--grace` are skipped (default 1h, set `0s` to disable).

## Storage layout

```
data/
  blobs/sha256/ab/cd/{hash}        content-addressed
  aliases/{namespace}/{path}.json  metadata, atomic temp+rename
  tmp/uploads/                     streaming temp files
auth.json                          atomic temp+rename, 0600
```

## Security notes

- Tokens are bearer secrets; only ship them over HTTPS.
- `auth.json` is written atomically with mode 0600 and stored only as SHA-256.
- PUT bodies stream directly to disk; nothing is buffered in memory.
- Object paths reject `..`, `%2e%2e`, backslashes, and null bytes; `path.Clean`
  is run on the server.
- Alias JSON is validated: the `hash` field must match `^[0-9a-f]{64}$` and
  the resolved blob path is verified to stay under `data/blobs/sha256/` even
  if an attacker somehow plants a hand-crafted alias on disk.
- Blobs are only reachable via aliases bound to a namespace; there is no
  by-hash endpoint, so knowing a content hash does not bypass namespace ACLs.
- When `-base-domain` is set, the base host with no subdomain never resolves
  to a namespace — only proper subdomains do.
- Run picocdn as an unprivileged user; mount the data directory `0700`.
- Put Caddy or another reverse proxy in front for TLS termination.

## Health and observability

- `GET /healthz` returns `{"status":"ok"}` for liveness checks.
- Each request is logged with `method`, `host`, `path`, `status`, `bytes`,
  `ms`, `remote`, and `ua` through `log/slog` text handler.

## Roadmap

- delete tombstones for replicated setups
- structured access log fan-out (file + journal)
- per-token rate limits
- resumable upload (tus.io / S3 multipart) for very large files
