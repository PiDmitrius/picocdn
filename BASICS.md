# picocdn — basics

Minimal recipes for an agent that already has a token. Full reference is in `README.md`.

New namespaces are **public-read** and serve **`index.html`** by default. Upload files, share the URL, done. To make a namespace private, see "Lock down" below.

## Setup

```sh
HOST=https://cdn.example.com    # base host of the deployment
NS=myapp                        # namespace name
TOKEN=prt_or_pcd_xxx            # root, owner, or sub token
auth() { curl -fsS -H "Authorization: Bearer $TOKEN" "$@"; }
```

## Root token (`prt_…`)

```sh
# create namespace; returns plaintext owner_token (save it)
auth -d "{\"name\":\"$NS\"}" "$HOST/_/namespaces"

# list namespaces
auth "$HOST/_/namespaces"

# delete namespace
auth -X DELETE "$HOST/_/namespaces/$NS"

# rotate owner token (old one stops working)
auth "$HOST/_/namespaces/$NS/rotate-owner"
```

## Owner token (`pcd_…`)

```sh
# upload file
auth -T file.bin "$HOST/$NS/path/to/file.bin"

# upload folder recursively (strips leading ./)
( cd ./dir && find . -type f -print0 \
  | while IFS= read -r -d '' f; do
      auth -T "$f" "$HOST/$NS/${f#./}" >/dev/null
    done )

# download
auth -o out.bin "$HOST/$NS/path/to/file.bin"

# delete object
auth -X DELETE "$HOST/$NS/path/to/file.bin"

# list (prefix optional, always needs a token)
auth "$HOST/$NS/?prefix=/path"

# issue sub-token (any subset of read/write/delete)
auth -X POST -d '{"name":"ci","permissions":["read","write"]}' \
     "$HOST/_/namespaces/$NS/tokens"

# list / revoke sub-tokens
auth "$HOST/_/namespaces/$NS/tokens"
auth -X DELETE "$HOST/_/namespaces/$NS/tokens/<token_id>"

# override the directory index file (default = index.html)
auth -X POST -d '{"file":"main.html"}' "$HOST/_/namespaces/$NS/index"
auth -X POST -d '{"file":""}'          "$HOST/_/namespaces/$NS/index"  # back to default
auth -X POST -d '{"disabled":true}'    "$HOST/_/namespaces/$NS/index"  # off
```

## Sub-token (`pcd_…`, scoped to one namespace)

Use only the object commands above that match your permissions:

- `read`   → download, list
- `write`  → upload
- `delete` → delete

Admin endpoints (`/_/…`) are not available.

## Lock down

By default the namespace is publicly readable. To make it private (anonymous GET stops working, every read needs a token):

```sh
auth -X POST -d '{"on":false}' "$HOST/_/namespaces/$NS/public"
```

Turn back on:

```sh
auth -X POST -d '{"on":true}'  "$HOST/_/namespaces/$NS/public"
```

Listing (`GET /$NS/?prefix=…`) always needs a token regardless of this flag.

## Object URL forms

These are equivalent:

```
$HOST/$NS/path/to/file
https://$NS.<base-domain>/path/to/file   # if subdomain routing is deployed
```
