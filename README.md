# picocdn

Small self-hosted CDN/storage service for AI-generated artifacts. Go stdlib only.

Authorization has three layers:

- **root** tokens live in `~/.config/picocdn/config.json` and have full access
  to the entire CDN. Root tokens are only managed via the CLI.
- **owner** tokens live inside `<data-dir>/namespaces/<ns>.json` and can manage
  their own namespace (issue sub-tokens, set public-read, etc.).
- **sub-tokens** carry an explicit subset of `read|write|delete` for one
  namespace.

All namespace operations after bootstrap (create namespace, issue tokens,
rotate owner, set-public, delete namespace) happen over HTTP under `/_/`.

## Quick start

```sh
picocdn install                # binary + systemd user unit
picocdn init                   # prints the first root token (save it!)
picocdn start                  # start the service

ROOT=prt_xxxxxxxxxxxxxxxxxxxxxxxxxxxx

# create a namespace; the response carries the namespace's owner token
curl -fsS -H "Authorization: Bearer $ROOT" \
     -d '{"name":"default"}' \
     http://127.0.0.1:8080/_/namespaces

OWNER=pcd_yyyyyyyyyyyyyyyyyyyyyyyyyyyy

# upload, download, list, delete with the owner token
curl -fsS -H "Authorization: Bearer $OWNER" \
     -T cat.png http://127.0.0.1:8080/default/docs/cat.png
```

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/PiDmitrius/picocdn/main/install.sh | bash
```

The installer places the binary in `~/.local/bin/picocdn` and prepares the user
environment for `systemd --user`.

```sh
picocdn install
picocdn init                   # bootstrap root token
picocdn start
```

The user service listens on `127.0.0.1:8080` by default, stores data in
`~/.local/share/picocdn`, and reads optional environment overrides from
`~/.config/picocdn/picocdn.env`.

`examples/picocdn.service` is a reference template; `picocdn install` writes
an equivalent user service with the installed binary path substituted.

Subdomain routing (`https://{namespace}.your-domain.tld/...`) is **opt-in** —
turn it on by passing `-base-domain your-domain.tld` and putting Caddy with
wildcard DNS in front. Out of the box you address objects through the path
under the listen port; both styles share the same verbs and tokens.

Configuration via env or flags:

| env                          | flag                | default                         |
|------------------------------|---------------------|---------------------------------|
| `PICOCDN_ADDR`               | `-addr`             | `:8080`                         |
| `PICOCDN_DATA_DIR`           | `-data-dir`         | `/var/lib/picocdn`              |
| `PICOCDN_BASE_DOMAIN`        | `-base-domain`      | _empty_ (subdomain routing off) |
| `PICOCDN_MAX_UPLOAD_BYTES`   | `-max-upload-bytes` | `1073741824`                    |

`config.json` is read once at startup; changes to root tokens take effect
after `picocdn restart`. Namespace files in `<data-dir>/namespaces/` are owned
by the running server — all mutations happen through the admin HTTP plane.

## CLI commands

```text
picocdn install       install binary and systemd user service
picocdn uninstall     remove systemd user service
picocdn start         start the service (--foreground to run directly)
picocdn stop          stop the service
picocdn restart       restart the service
picocdn status        show service status
picocdn update        update from GitHub release or rebuild from source_dir
picocdn fallback      install latest or selected GitHub release
picocdn config        show or edit local config (source_dir)
picocdn init          create the first root token (fails if one already exists)
picocdn root          create / list / revoke root tokens
picocdn gc            remove unreferenced blobs
picocdn backup        write backup tarball
picocdn restore       restore backup tarball
picocdn version       print version
```

Anything else (namespaces, sub-tokens, public-read, owner rotation) is
operated over the HTTP admin plane; the CLI no longer mutates that state.

## Root tokens

Root tokens are managed locally via the CLI. They never touch HTTP for
creation or revocation — compromise of the HTTP plane cannot escalate to
issuing new roots.

```sh
picocdn init                          # first root only; fails if non-empty
picocdn root create --name laptop     # issue another root
picocdn root list                     # ids / names / created_at (no plaintext)
picocdn root revoke <token_id>        # refuses to remove the last one
```

After any root-token mutation, run `picocdn restart` so the server picks up
the change. Root token plaintext is printed once at creation; only the SHA-256
hash is persisted in `~/.config/picocdn/config.json` (mode 0600).

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
GET    http://host/{ns}[?prefix=/x]       list objects in a namespace

GET    /healthz                            liveness (auth-free)
```

### Admin plane

All admin operations live under `/_/`. The leading `_` is structurally
unreachable as a namespace name (namespaces must start with a letter), so
admin paths never collide with object paths.

```
POST   /_/namespaces                              root only
  body: {"name": "myapp"}
  response: {"namespace":"myapp", "owner_token_id":"...", "owner_token":"pcd_..."}
  201 / 409 on conflict / 400 on invalid name

GET    /_/namespaces                              root only
  response: [{"name":"...", "owner_token_id":"...", "public_read":false, ...}]

DELETE /_/namespaces/{ns}                         root only
  204; alias directory is removed, blobs reclaimed by gc

POST   /_/namespaces/{ns}/rotate-owner            root only
  response: {"owner_token_id":"...", "owner_token":"pcd_..."}

POST   /_/namespaces/{ns}/tokens                  owner or root
  body: {"name":"ci", "permissions":["read","write"]}
  response: {"token_id":"...", "token":"pcd_...", "permissions":[...]}
  "owner" is not accepted here; use rotate-owner

GET    /_/namespaces/{ns}/tokens                  owner or root
  response: [{"id":"...", "name":"...", "permissions":[...], "owner":bool, ...}]

DELETE /_/namespaces/{ns}/tokens/{id}             owner or root
  204; owner token cannot be removed this way, use rotate-owner instead

POST   /_/namespaces/{ns}/public                  owner or root
  body: {"on": true|false}

POST   /_/namespaces/{ns}/index                   owner or root
  body: {"file": "main.html"}     override the directory-index filename
  body: {"file": ""}              reset to default ("index.html")
  body: {"disabled": true}        turn the directory index off entirely
  body: {"disabled": false}       turn it back on (uses default unless file is set)
```

Namespace names must match `^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$` so they stay
valid DNS labels for optional subdomain routing.

### Subdomain routing (optional)

Run `picocdn serve -base-domain your-domain.tld` and the same object handlers
also respond to `https://{ns}.your-domain.tld/...`. The namespace then comes
from the subdomain instead of the first URL segment; the verbs and the auth
model are unchanged. Admin paths under `/_/` always go to the base host.

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
cdn -T cat.png $HOST/docs/cat.png
cdn -T blob -H 'Content-Type: application/x-protobuf' $HOST/raw/blob
cdn --progress-bar -T dump.tar.zst $HOST/dumps/dump.tar.zst
tar c src/ | cdn -T - $HOST/snapshots/src.tar
```

### Download

```sh
cdn -o cat.png $HOST/docs/cat.png
cdn --range 0-1023 -o head.bin $HOST/big.bin
cdn --continue-at - -o dump.tar.zst $HOST/dumps/dump.tar.zst
cdn -I $HOST/docs/cat.png            # HEAD
```

Conditional GET — 304 if ETag matches:

```sh
ETAG=$(cdn -I $HOST/docs/cat.png | awk -F': ' 'tolower($1)=="etag"{print $2}' | tr -d '\r')
cdn -i -H "If-None-Match: $ETAG" $HOST/docs/cat.png | head -1
```

### Delete and list

```sh
cdn -X DELETE $HOST/docs/cat.png
cdn "$HOST/?prefix=/docs" | jq
```

## Update flow

`picocdn update` behaves in one of two ways:

- If `source_dir` is empty, it downloads the latest GitHub release and installs it.
- If `source_dir` is set, it bumps the local patch version, rebuilds from source,
  and installs that binary instead.

After a source update, commit the version bump; when it is ready for release,
push a `v*` tag to publish binaries.

```sh
picocdn config set-source-dir "$(pwd)"
picocdn update

picocdn fallback          # latest release
picocdn fallback v0.1.0   # selected release
```

## Public-read namespaces

```sh
curl -fsS -H "Authorization: Bearer $OWNER" \
     -d '{"on":true}'  http://127.0.0.1:8080/_/namespaces/default/public
curl -fsS -H "Authorization: Bearer $OWNER" \
     -d '{"on":false}' http://127.0.0.1:8080/_/namespaces/default/public
```

When public-read is enabled, `GET`/`HEAD` on individual objects work without
a token. **Listing always requires a token** even on public-read namespaces.
`PUT`/`DELETE` still require the relevant permission. Cache-Control is relaxed
to `public, max-age=300` for objects served from public namespaces.

## Directory index

By default every namespace serves `index.html` when a request lands on the
namespace root or any path ending with `/`. Upload an `index.html` and it just
works — no admin call needed.

```sh
# override the filename for a specific namespace
curl -fsS -H "Authorization: Bearer $OWNER" \
     -d '{"file":"main.html"}' \
     http://127.0.0.1:8080/_/namespaces/site/index

# reset back to the "index.html" default
curl -fsS -H "Authorization: Bearer $OWNER" \
     -d '{"file":""}' \
     http://127.0.0.1:8080/_/namespaces/site/index

# turn the directory index off entirely
curl -fsS -H "Authorization: Bearer $OWNER" \
     -d '{"disabled":true}' \
     http://127.0.0.1:8080/_/namespaces/site/index
```

Effective lookup:

- `GET https://site.example.com/`          → serves `/index.html` (or override)
- `GET https://site.example.com/blog/`     → serves `/blog/index.html`
- `GET https://example.com/site/`          → serves `/site/index.html` (path fallback)
- `GET https://site.example.com/missing/`  → 404 (no `missing/index.html` on disk)

When the index is disabled, `GET /{ns}/` returns the listing response
(which always requires a token, even on public-read namespaces).

Override filename validation: ASCII letters/digits/`.`/`_`/`-`, no `/`,
no `..`, no `.`. The index file is served through the same code path as a
regular `GET`, so `public_read`, ETag, Range, and Cache-Control all apply
uniformly.

## Backup, restore, GC

```sh
picocdn backup  --data-dir ./data --out ./pico.tgz
picocdn restore --data-dir ./data --in  ./pico.tgz [--force]
picocdn gc      --data-dir ./data [--grace 1h]
```

- The backup tarball includes `config.json` and `data/{namespaces,blobs,aliases}/`.
- `restore` refuses to overwrite a non-empty target unless `--force` is given.
- `gc` walks every alias, then deletes any blob no alias references. Files
  newer than `--grace` are skipped (default 1h, set `0s` to disable).

## Storage layout

```
~/.config/picocdn/config.json         root config + root token hashes, 0600
<data-dir>/
  namespaces/{name}.json              namespace tokens, atomic temp+rename, 0600
  blobs/sha256/ab/cd/{hash}           content-addressed
  aliases/{namespace}/{path}.json     metadata, atomic temp+rename
  tmp/uploads/                        streaming temp files
```

## Security notes

- Tokens are bearer secrets; only ship them over HTTPS via a reverse proxy.
- `config.json` and `namespaces/<ns>.json` are written atomically with mode
  0600 and store only SHA-256 token hashes; plaintext is printed once.
- Root token plaintext never leaves the host that ran `picocdn init`/
  `picocdn root create`.
- HTTP admin plane never escalates privilege: it can issue namespace owner
  tokens but cannot create or modify root tokens.
- PUT bodies stream directly to disk; nothing is buffered in memory.
- Object paths reject `..`, `%2e%2e`, backslashes, and null bytes; `path.Clean`
  is run on the server.
- Blobs are only reachable via aliases bound to a namespace; there is no
  by-hash endpoint, so knowing a content hash does not bypass namespace ACLs.
- A broken `namespaces/<ns>.json` at startup fails the server fast rather than
  silently losing a namespace.
- When `-base-domain` is set, the base host with no subdomain never resolves
  to a namespace, and a subdomain whose first label starts with `_` is never
  routed as a namespace.
- Run picocdn as an unprivileged user; mount the data directory `0700`.
- Put Caddy or another reverse proxy in front for TLS termination.

## Health and observability

- `GET /healthz` returns `{"status":"ok"}` for liveness checks.
- Each request is logged with `method`, `host`, `path`, `status`, `bytes`,
  `ms`, `actor` (`root:id` / `owner:id` / `sub:id` / `anon`), `remote`, and
  `ua` through `log/slog`'s text handler.

## Roadmap

- structured access log fan-out (file + journal)
- per-token rate limits
- resumable upload (tus.io / S3 multipart) for very large files
- `/_/metrics` via stdlib `expvar`
