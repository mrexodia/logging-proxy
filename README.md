# Logging Proxy

High performance HTTP reverse proxy server. Built in Go, this proxy allows you to route API requests to multiple destinations while capturing full request/response data for analysis and debugging.

It was built to capture LLM traces for OpenRouter, without having to set up heavy enterprise routers like [LiteLLM](https://github.com/BerriAI/litellm).

## Architecture

The package consists of three components components:

1. **Proxy Package** (`server.go`): Routes requests and handles streaming
2. **Logging Proxy Server** (`logging-proxy/`): Command line tool for the proxy that logs the requests

## Configuration

Edit `config.yaml` to configure the proxy:

```yaml
server:
  port: 5601
  host: "localhost"
  not_found: "/404/"

logging:
  console: true           # Enable console output for request monitoring
  server_url: "http://localhost:8080"  # Logging server URL
  default: true           # Default logging behavior for routes and unknown requests

routes:
  openrouter:
    pattern: "/api/v1/"
    destination: "https://openrouter.ai/"
  lmstudio:
    pattern: "/lmstudio/"
    destination: "http://127.0.0.1:1234/"
    logging: false        # Disable logging for this route
```

### Configuration Options

- **server.port**: Port for the proxy server (default: 5601)
- **server.host**: Host interface to bind to (default: localhost)
- **logging.console**: Enable/disable console request monitoring
- **logging.server_url**: URL of the logging server
- **logging.default**: Log unknown routes and 404 responses
- **routes**: Map of route configurations with pattern/destination mappings

## Running the Application

1. **Start the logging proxy server**:
   ```bash
   go run ./logging-proxy
   ```
   The proxy will start on the configured port (default: `5601`).

## Testing

### Running Unit Tests

```bash
go test -v .
```

### Manual Testing with test.http

The project includes `test.http` with example requests for manual testing using [VS Code REST Client](https://marketplace.visualstudio.com/items?itemName=humao.rest-client).

**Example test scenarios:**

1. **Direct LM Studio Request** (for comparison):
   ```http
   POST http://127.0.0.1:1234/v1/chat/completions
   Content-Type: application/json
   Authorization: Bearer sk-xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
   
   {
     "model": "liquid/lfm2-1.2b",
     "messages": [{"role": "user", "content": "Test message"}]
   }
   ```

2. **Proxied Request**:
   ```http
   POST http://127.0.0.1:5601/lmstudio/v1/chat/completions
   Content-Type: application/json
   Authorization: Bearer sk-xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
   
   {
     "model": "liquid/lfm2-1.2b",
     "messages": [{"role": "user", "content": "Test message"}]
   }
   ```

3. **Streaming Request**:
   ```http
   POST http://127.0.0.1:5601/lmstudio/v1/chat/completions
   Content-Type: application/json
   
   {
     "model": "liquid/lfm2-1.2b",
     "messages": [{"role": "user", "content": "Test message"}],
     "stream": true
   }
   ```

### Testing with LM Studio

1. **Setup LM Studio**:
   - Install and start LM Studio
   - Load a model (e.g., "liquid/lfm2-1.2b")
   - Enable the local server on `http://127.0.0.1:1234`

2. **Test the proxy**:
   - Start both logging server and proxy
   - Use the provided `test.http` requests
   - Compare direct vs. proxied responses
   - Check the `logs/` directory for captured traffic

## How It Works

### Request Flow

1. **Client** sends request to proxy (e.g., `localhost:5601/lmstudio/v1/chat/completions`)
2. **Proxy** matches the route (`/lmstudio/` → `http://127.0.0.1:1234/`)
3. **Path transformation** converts `/lmstudio/v1/chat/completions` → `/v1/chat/completions`
4. **Duplex streaming** forwards request to destination while logging (if enabled)
5. **Response streaming** returns data to client while logging response
6. **Logging server** stores complete HTTP request/response data with metadata

### Route Matching

Routes use Go's [http.ServeMux](https://pkg.go.dev/net/http#hdr-Patterns-ServeMux) pattern matching:

**Pattern Types:**
- `/lmstudio/` - Matches `/lmstudio/` and all subpaths
- `GET /lmstudio/file.txt` - Matches exactly `/lmstudio/file.txt`, no subpaths, just the `GET` method
- `GET example.com/test/{$}` - Matches `Host: example.com`, path `/test` and `/test/`, but not `/test/foo`
- `POST example.com/test/` - Matches `Host: example.com` and anything under `/test/`
- `"/"` - Catch-all that matches everything

**Note**: Wildcards (except `{$}`) are **not** supported and will be rejected on startup.

**Example:**
- Request: `/lmstudio/v1/chat/completions`
- Pattern: `/lmstudio/` → `http://127.0.0.1:1234/`
- Result: `http://127.0.0.1:1234/v1/chat/completions`

In general, more specific patterns win when multiple patterns could match. If you create identical patterns the proxy will panic on startup.

### Logging Format

Captured logs include:
- **Binary files**: Complete HTTP request/response data
- **Metadata JSON**: Request ID, timestamps, headers, processing time
- **X-Proxy-Path header**: Original proxy URL for replay capability

Log files are named: `{timestamp}_{requestID}_{request|response}.bin`

## Console Output

When `logging.console` is enabled, you'll see real-time request monitoring:

```
2025-09-13 02:11:09 [092d0424] POST /lmstudio/v1/chat/completions -> http://127.0.0.1:1234/v1/chat/completions [log]
```

## Future Enhancements

- Metadata endpoint for querying logged requests
- Web-based logging UI with live request feed
- WebSocket support for real-time monitoring
- Custom Transport implementation for simplified logging

## Build for Linux

```bash
cd logging-proxy
GOOS=linux GOARCH=amd64 go build .
```