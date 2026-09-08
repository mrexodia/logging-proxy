# Logging Proxy

HTTP proxy tooling for capturing full request/response traffic.

The binary can run two listener types:

1. **Reverse proxy** from `server:` + `routes:`
2. **Forward proxy** from `proxy:` for `HTTP_PROXY` / `HTTPS_PROXY`

Outbound requests from either listener can use an upstream client proxy from `http_client:` or from `HTTP_PROXY`, `HTTPS_PROXY`, and `NO_PROXY`.

At least one of `server:` or `proxy:` must be configured. If `proxy:` is omitted, only the reverse proxy starts. If `server:` is omitted, only the forward proxy starts.

## Reverse proxy

When `server:` is configured, the reverse proxy listens on `server.host:server.port` and routes requests using `routes`.

Example:

```yaml
server:
  port: 5601
  host: "localhost"
  not_found: "/404/"

logging:
  enabled: true
  console: true
  log_dir: "logs"

# Optional. proxy_url overrides environment proxy variables.
http_client:
  proxy_url: "http://127.0.0.1:3128"

routes:
  # OPENAI_BASE_URL=http://localhost:5601/openrouter
  openrouter:
    pattern: "/openrouter/"
    destination: "https://openrouter.ai/api/v1/"
    authorization:
      backend_key: "${OPENROUTER_API_KEY}"
      template: "Bearer {}"
      # Optional: require a different credential from local clients.
      # client_key: "${LOCAL_PROXY_ACCESS_KEY}"
  openrouter_models:
    pattern: "/openrouter/models/"
    destination: "https://openrouter.ai/api/v1/models/"
    logging: false
  # OPENAI_BASE_URL=http://localhost:5601/lmstudio
  lmstudio:
    pattern: "/lmstudio/"
    destination: "http://127.0.0.1:1234/v1/"
  # ANTHROPIC_BASE_URL=http://localhost:5601/anthropic
  anthropic:
    pattern: "/anthropic/"
    destination: "https://api.anthropic.com/"
  # OPENAI_BASE_URL=http://localhost:5601/llama.cpp
  llama.cpp:
    pattern: "/llama.cpp/"
    destination: "http://127.0.0.1:8080/v1/"
```

### Reverse proxy route authorization

A route can inject an authorization credential into its backend requests:

```yaml
routes:
  openrouter:
    pattern: "/openrouter/"
    destination: "https://openrouter.ai/api/v1/"
    authorization:
      header: "Authorization"                 # Default: Authorization
      backend_key: "${OPENROUTER_API_KEY}"    # Optional if client_key is set
      template: "Bearer {}"                    # Default for Authorization
      client_key: "${LOCAL_PROXY_ACCESS_KEY}"  # Optional
```

Keys may be literal values or exact `${VARIABLE}` references. Variables are read from the process environment first, then from an optional `.env` file beside the selected config file. Existing process variables override `.env`; a referenced variable that is missing or empty causes startup to fail.

For the `Authorization` header, `template` defaults to `Bearer {}`. Other header names require an explicit template such as `{}` for a raw API key. A template must contain exactly one literal `{}` and formats both keys. At least one key is required:

- `backend_key` only: inject the formatted backend credential without authenticating local clients.
- Both keys: validate the formatted client credential, then replace it with the backend credential.
- `client_key` only: validate the formatted client credential, then remove that header before forwarding.

A missing or incorrect client credential returns `401` without calling the backend. Outgoing request logs contain the authorization header actually sent upstream.

Example `.env`:

```dotenv
OPENROUTER_API_KEY=sk-backend-secret
LOCAL_PROXY_ACCESS_KEY=local-client-secret
```

## Outbound client proxy

Use `http_client.proxy_url` to route outbound requests through a specific upstream proxy:

```yaml
http_client:
  proxy_url: "http://127.0.0.1:3128"
```

SOCKS proxies are also supported:

```yaml
http_client:
  proxy_url: "socks5://127.0.0.1:1080"
```

`proxy_url` overrides environment proxy variables. If `proxy_url` is empty, `proxy_from_environment` defaults to `true`, so `HTTP_PROXY`, `HTTPS_PROXY`, `ALL_PROXY`, and `NO_PROXY` (or their lowercase forms) from the logging-proxy process environment are honored. HTTP destinations use `HTTP_PROXY`, HTTPS destinations use `HTTPS_PROXY`, and `ALL_PROXY` fills either scheme-specific setting when it is absent. `NO_PROXY` bypasses the selected proxy for matching destinations. Localhost and loopback destinations are always bypassed. These variables apply to outbound traffic from both reverse and forward proxy listeners and may contain `socks5://` or `socks5h://` URLs.

The adjacent `.env` file is used for explicit `${VARIABLE}` config references; it does not export `HTTP_PROXY`, `HTTPS_PROXY`, `ALL_PROXY`, or `NO_PROXY` into the process environment. An inherited proxy URL that points back to this logging-proxy process is rejected at startup to prevent a proxy loop. Set `proxy_from_environment` to `false` to force direct outbound connections:

```yaml
http_client:
  proxy_from_environment: false
```

## Forward proxy

If `proxy:` is present, the same binary also starts a forward proxy listener.

Example:

```yaml
proxy:
  port: 8080
  host: "127.0.0.1"
  verbose: false
  auth:
    username: "${FORWARD_PROXY_USERNAME}"
    password: "${FORWARD_PROXY_PASSWORD}"
  mitm:
    enabled: true
    certs_dir: "certs"
    organization: "logging-proxy"
    # Hostname used in the CRL URL. Required when proxy.host is 0.0.0.0, ::, or empty.
    hostname: "proxy.example.lan"
    logging_exclude_url_prefixes:
      # Trailing slash matches everything below this prefix.
      - "https://openrouter.ai/api/v1/models/"
      # Without trailing slash, only that endpoint is skipped.
      - "https://openrouter.ai/api/v1/models"
    # Optional allow-list. If present, only matching hosts are captured.
    include_hosts:
      - "api.anthropic.com"
      - "*.example.com"
    exclude_hosts:
      - "*.bank.example"
      - "10.0.0.0/8"
```

Forward proxy behavior:
- Plain HTTP requests are logged directly unless filtered out by `proxy.mitm.include_hosts`
- HTTPS without MITM is tunneled with CONNECT, so bodies are encrypted
- HTTPS with MITM decrypts and logs request/response bodies

`proxy.auth` is optional. Its username and password may be literal values or exact `${VARIABLE}` references resolved from the process environment and adjacent `.env` file. When configured, clients must use HTTP Basic proxy authentication, for example `HTTP_PROXY=http://proxy-user:proxy-password@127.0.0.1:8080`.

MITM creates or loads a persistent root/intermediate CA hierarchy in `proxy.mitm.certs_dir`. Trust `root-ca.crt` once on clients. Leaf certificates are signed by the intermediate CA, expire after 24 hours, and include a CRL distribution point at `http://<hostname>:<proxy-port>/crl`. `proxy.mitm.hostname` overrides the CRL hostname and is required when `proxy.host` is `0.0.0.0`, `::`, or empty.

`proxy.mitm.logging_exclude_url_prefixes` skips disk logging for matching decrypted URLs while still forwarding the requests. Entries are absolute `http://` or `https://` URLs. A path ending in `/` matches everything below that prefix; a path without trailing `/` matches only that endpoint.

`proxy.mitm.include_hosts` is an optional allow-list. If it is non-empty, only matching hosts are MITM-decrypted/logged; non-matching HTTPS hosts fall back to opaque CONNECT tunneling, and non-matching plain HTTP proxy requests are forwarded without logging.

`proxy.mitm.exclude_hosts` disables capture for matching hosts: HTTPS falls back to opaque CONNECT tunneling and plain HTTP proxy requests are forwarded without logging. If both include and exclude match, exclude wins. Entries support exact hosts, `*.example.com` suffix wildcards, IP literals, CIDR ranges, and `*`.

## Running

```bash
go run ./logging-proxy
```

With both `server:` and `proxy:` present, both listeners start.

## Live web UI

Start the read-only inspector **alongside an already running proxy**:

```bash
go run ./cmd/proxy-webui -log-dir logs -port 5602
```

Open **http://127.0.0.1:5602**. The standalone viewer does not load proxy config,
start proxy listeners, create a logger, or restart the running proxy. It opens an
existing log directory for reading only. No migration, cleanup, retention limit,
rewrite, or deletion is performed. Run it from the proxy's working directory or
pass an absolute `-log-dir` path. To build a standalone Windows executable:

```powershell
go build -o proxy-webui.exe ./cmd/proxy-webui
.\proxy-webui.exe -log-dir logs -port 5602
```

The interface includes:

- Live request arrivals and updates, with searchable, paginated history across
  every transaction metadata file in the selected directory.
- Request and response headers, bodies, lifecycle metadata, status and sizes.
- SSE event decoding and bidirectional WebSocket frame inspection. Pause/resume
  and reconnect continue from byte offsets in the original capture files.
- Raw text and hex pages, plus full original-file downloads. Live previews retain
  up to 2 MiB of text and 2 MiB / 300 decoded events; preview limits never affect
  disk capture. Raw text pages may split a UTF-8 character at a page boundary;
  hex pages and downloads preserve exact bytes.
- An **All files** view with metadata, orphaned `.bin` captures, and `.zip`
  archives. ZIPs are downloadable as originals; their contents are not indexed.

JSONL and older per-stream JSON metadata are read directly. If both formats
describe the same request ID, JSONL is preferred without deleting either file.
Malformed/partial metadata remains downloadable and is flagged in the UI.
History has no age or count cutoff. The viewer scans the directory once per
second, caches unchanged completed metadata, and tails selected streams every
250 ms. It does not recursively inspect subdirectories.

**Unfinished** means there is no recorded completion for both streams. It can
mean a live request, an upstream failure with no response capture, or a stream
interrupted by a previous process exit. File watching cannot distinguish all
three. Requests excluded by existing logging settings and opaque CONNECT
tunnels have no disk capture to show.

WebSocket capture is supported by the updated reverse and forward proxies
(including HTTPS MITM). Each direction's existing `.bin` format contains the
HTTP handshake followed by the exact WebSocket frames. Masked frames are
unmasked for preview; binary and compressed frame payloads are shown in hex.
Fragmented messages are shown frame by frame. The protocol follows
[RFC 6455](https://www.rfc-editor.org/rfc/rfc6455.html). An older running proxy
continues to supply its existing HTTP/SSE captures to the viewer, but needs a
separately scheduled restart with the updated binary to gain WebSocket capture.
Starting the viewer alone does not change that running binary.

To start the inspector automatically with the proxy on a future normal startup,
add this optional section to your config:

```yaml
webui:
  host: "127.0.0.1"
  port: 5602
```

Do not run the standalone and integrated viewers on the same port. The UI only
accepts loopback listener addresses (`127.0.0.1`, `::1`, or `localhost`), rejects
foreign Host/Origin headers, and loads no external scripts. Captures can include
authorization headers and private bodies; use an authenticated tunnel if remote
access is needed.

### Read-only HTTP API

All API routes use GET and return JSON except downloads and SSE streams:

| Route | Behavior |
| --- | --- |
| `/api/requests?q=&state=&method=&offset=0&limit=100` | Newest-first metadata history; `state` is `completed`, `unfinished`, or `error`. Maximum page size is 500. |
| `/api/requests/{key}` | Full transaction lifecycle and capture filenames. |
| `/api/files?q=&offset=0&limit=100` | All recognized capture files and archives. |
| `/api/file?name=…&offset=0` | Up to 64 KiB, base64 encoded, with byte offsets and current size. |
| `/api/file?name=…&download=1` | Original bytes, with HTTP Range support. An active download reflects the size when it starts. |
| `/api/events` | SSE `change` notifications with an index revision; refetch history after each new revision or reconnect. |
| `/api/tail?name=…&offset=0` | SSE `chunk` events with base64 bytes and offsets, followed by `end` after recorded stream completion. |

Tail event IDs are the next byte offset. `Last-Event-ID` takes precedence over
the `offset` query argument when reconnecting. Slow or disconnected viewers
replay from disk; they never block a proxy request or cause capture eviction.
Keep-alive notifications are sent at least every 15 seconds while waiting. A
stream without a completion record remains open until the viewer disconnects.

## MITM client setup

For HTTPS body capture, enable `proxy.mitm.enabled` and trust the generated root CA:

```bash
export HTTP_PROXY=http://127.0.0.1:8080
export HTTPS_PROXY=http://127.0.0.1:8080
# Add /absolute/path/to/certs/root-ca.crt to the client's trust store.
```

Without MITM, HTTPS bodies are not visible.

## Logging

Logs are written to `logging.log_dir`.

Captured files:
- `*_request.bin`
- `*_response.bin`
- `*_metadata.jsonl`

Each request/response transaction has one append-only metadata JSONL. It records
`request_started`, `request_completed`, `response_started`, and
`response_completed` events as they happen. Request and response events may
interleave according to their actual completion order. Every line is a complete
JSON object, so filesystem watchers can tail complete lines while a request is
still active; after a crash, an unmatched `*_started` event identifies an
incomplete stream.

For MITM HTTPS requests, the `.bin` files contain decrypted HTTP headers and bodies.

### Migrating legacy metadata

`scripts/migrate_metadata_jsonl.py` converts the previous per-stream
`*_request_metadata.json` and `*_response_metadata.json` files into one
transaction JSONL. Body `.bin` files are never modified. Stop the proxy or
migrate a copy of the log directory so legacy metadata cannot change during the
conversion.

```bash
# Validate and preview only.
python scripts/migrate_metadata_jsonl.py logs --dry-run

# Write JSONL while retaining the old metadata files (default).
python scripts/migrate_metadata_jsonl.py logs

# Remove old metadata JSON after each JSONL is written successfully.
python scripts/migrate_metadata_jsonl.py logs --delete-old
```

Existing JSONL files are skipped unless `--overwrite` is supplied. Historical
metadata versions without completion fields are treated as completed when their
referenced `.bin` exists; an explicit `completed: false` remains an unmatched
started event.

The generated JSONL also preserves the source metadata's filesystem dates by
default. Because two source files become one, it uses the earliest creation time
(on Windows) and the latest modification/access time. Use
`--no-preserve-file-times` to timestamp the JSONL at migration time instead.

If an earlier migration already removed the legacy metadata, the dates can still
be reconstructed from the JSONL lifecycle timestamps:

```bash
python scripts/migrate_metadata_jsonl.py logs --repair-file-times --dry-run
python scripts/migrate_metadata_jsonl.py logs --repair-file-times
```

Repair mode uses the earliest started event as the creation time and the latest
event as the modification/access time. Stop the proxy before repairing active
JSONL files.

## Library integrations

The proxy can be embedded as a Go library with `NewProxyServer` or
`NewHTTPProxyServer`. Implement `Logger` to consume each request and response
stream as it arrives. This leaves room for future in-memory observers or live
streaming integrations without tying them to the on-disk format. Alternatively,
external tools can watch the metadata JSONL and growing `.bin` files.

Custom loggers are capture-enabled by default. A logger that implements
`CaptureController` and returns `false` is removed from the request hot path
entirely. `NoOpLogger` does this automatically, so disabling logging avoids
capture pipes, header reconstruction, and logging goroutines.

## Reverse proxy route matching

Routes use Go `http.ServeMux` patterns.

Examples:
- `/lmstudio/` matches everything below `/lmstudio/`
- `/exact` matches only `/exact`
- `/` is a catch-all

Go `http.ServeMux` supports wildcards, but this proxy currently rejects named wildcards in configured route patterns (for example `{id}` and `{path...}`). The special `{$}` end-anchor is still allowed.

## Testing

```bash
go test ./...
node --test webui/decoders_test.mjs
```

## Build for Linux

```bash
cd logging-proxy
GOOS=linux GOARCH=amd64 go build .
```
