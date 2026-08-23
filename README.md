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
```

## Build for Linux

```bash
cd logging-proxy
GOOS=linux GOARCH=amd64 go build .
```