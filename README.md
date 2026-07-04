# Logging Proxy

HTTP proxy tooling for capturing full request/response traffic.

The binary can run two listeners at the same time:

1. **Reverse proxy** from `server:` + `routes:`
2. **Optional forward proxy** from `proxy:` for `HTTP_PROXY` / `HTTPS_PROXY`

Outbound requests from either listener can use an upstream client proxy from `http_client:` or from `HTTP_PROXY`, `HTTPS_PROXY`, and `NO_PROXY`.

If `proxy:` is omitted, only the reverse proxy is started.

## Reverse proxy

The reverse proxy listens on `server.host:server.port` and routes requests using `routes`.

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

`proxy_url` overrides environment proxy variables. If `proxy_url` is empty, `proxy_from_environment` defaults to `true`, so `HTTP_PROXY`, `HTTPS_PROXY`, and `NO_PROXY` are honored. These environment variables may also contain `socks5://` or `socks5h://` URLs. Set it to `false` to force direct outbound connections:

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
    username: "proxy-user"
    password: "proxy-password"
  mitm:
    enabled: true
    cert_file: "certs/mitm-ca-cert.pem"
    key_file: "certs/mitm-ca-key.pem"
    common_name: "logging-proxy MITM CA"
    organization: "logging-proxy"
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

`proxy.auth` is optional. When configured, clients must use HTTP Basic proxy authentication, for example `HTTP_PROXY=http://proxy-user:proxy-password@127.0.0.1:8080`.

If the MITM CA files do not exist, they are generated automatically.

`proxy.mitm.include_hosts` is an optional allow-list. If it is non-empty, only matching hosts are MITM-decrypted/logged; non-matching HTTPS hosts fall back to opaque CONNECT tunneling, and non-matching plain HTTP proxy requests are forwarded without logging.

`proxy.mitm.exclude_hosts` disables capture for matching hosts: HTTPS falls back to opaque CONNECT tunneling and plain HTTP proxy requests are forwarded without logging. If both include and exclude match, exclude wins. Entries support exact hosts, `*.example.com` suffix wildcards, IP literals, CIDR ranges, and `*`.

## Running

```bash
go run ./logging-proxy
```

With `proxy:` present, both listeners start.

## Claude Code setup

For HTTPS body capture, enable `proxy.mitm.enabled` and trust the generated CA:

```bash
export HTTP_PROXY=http://127.0.0.1:8080
export HTTPS_PROXY=http://127.0.0.1:8080
export NODE_EXTRA_CA_CERTS=/absolute/path/to/certs/mitm-ca-cert.pem
```

Without MITM, HTTPS bodies are not visible.

## Logging

Logs are written to `logging.log_dir`.

Captured files:
- `*_request.bin`
- `*_response.bin`
- `*_request_metadata.json`
- `*_response_metadata.json`

For MITM HTTPS requests, the `.bin` files contain decrypted HTTP headers and bodies.

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