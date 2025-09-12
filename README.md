# Logging Proxy

A high-performance HTTP proxy server with duplex streaming support and comprehensive logging capabilities. Built in Go, this proxy allows you to route API requests to multiple destinations while capturing full request/response data for analysis and debugging.

## Features

- **Duplex Streaming**: Real-time bidirectional streaming support for API requests
- **Route-Based Proxying**: Configure multiple routes with different destinations
- **Comprehensive Logging**: Optional per-route logging with request/response capture
- **Console Monitoring**: Real-time request tracking in the console
- **Request Replay**: Captured logs include original proxy paths for easy replay
- **Unique Request IDs**: Every request gets a UUID for correlation
- **Flexible Configuration**: YAML-based configuration with per-route settings

## Architecture

The package consists of three components components:

1. **Proxy Package** (`server.go`): Routes requests and handles streaming
2. **Logging Server** (`logging-server/`): Command line tool for logging data
2. **Proxy Server** (`logging-proxy/`): Command line tool for the proxy

## Configuration

Edit `config.yaml` to configure the proxy:

```yaml
server:
  port: 5601
  host: "localhost"

logging:
  console: true           # Enable console output for request monitoring
  server_url: "http://localhost:8080"  # Logging server URL
  default: true           # Default logging behavior for routes and unknown requests

routes:
  openrouter:
    source: "/api/v1/"
    destination: "https://openrouter.ai/"
  lmstudio:
    source: "/lmstudio"
    destination: "http://127.0.0.1:1234/"
    logging: false        # Disable logging for this route
```

### Configuration Options

- **server.port**: Port for the proxy server (default: 5601)
- **server.host**: Host interface to bind to (default: localhost)
- **logging.console**: Enable/disable console request monitoring
- **logging.server_url**: URL of the logging server
- **logging.default**: Log unknown routes and 404 responses
- **routes**: Map of route configurations with source/destination mappings

## Running the Application

1. **Start the logging server** (in one terminal):
   ```bash
   go run ./logging-server
   ```
   The logging server will start on port 8080 and create a `logs/` directory.

2. **Start the proxy server** (in another terminal):
   ```bash
   go run ./logging-proxy
   ```
   The proxy will start on the configured port (default: 5601).

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
2. **Proxy** matches the route (`/lmstudio` → `http://127.0.0.1:1234/`)
3. **Path transformation** converts `/lmstudio/v1/chat/completions` → `/v1/chat/completions`
4. **Duplex streaming** forwards request to destination while logging (if enabled)
5. **Response streaming** returns data to client while logging response
6. **Logging server** stores complete HTTP request/response data with metadata

### Route Matching

Routes use prefix matching with longest match wins:
- Request: `/lmstudio/v1/chat/completions`
- Route: `/lmstudio` → `http://127.0.0.1:1234/`
- Result: `http://127.0.0.1:1234/v1/chat/completions`

### Logging Format

Captured logs include:
- **Binary files**: Complete HTTP request/response data
- **Metadata JSON**: Request ID, timestamps, headers, processing time
- **X-Proxy-Path header**: Original proxy URL for replay capability

Log files are named: `{timestamp}_{requestID}_{request|response}.bin`

## Console Output

When `logging.console` is enabled, you'll see real-time request monitoring:

```
2024-01-15 10:30:45 [a1b2c3d4] POST /lmstudio/v1/chat/completions -> http://127.0.0.1:1234/ [log]
2024-01-15 10:30:46 [a1b2c3d4] Response completed (1.2s)
```

## Future Enhancements

- Metadata endpoint for querying logged requests
- Web-based logging UI with live request feed
- WebSocket support for real-time monitoring
- Custom Transport implementation for simplified logging
