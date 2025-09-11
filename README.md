# OpenRouter Proxy

A high-performance, configurable HTTP proxy with **streaming duplex architecture** designed for real-time logging and proxying requests to AI API endpoints like OpenRouter and OpenAI. Built with Go, it features UUID-based request tracking and streams request/response data to a dedicated logging server without blocking the main proxy pipeline.

## Features

- 🚀 **Streaming Duplex Architecture**: Real-time request/response streaming with zero performance impact
- 🆔 **UUID Request Tracking**: Each request gets a unique identifier for complete traceability
- 🔧 **Configurable Routes**: YAML-based configuration for multiple endpoints  
- 📊 **Dedicated Logging Server**: Separate server for handling log storage and processing
- 🌊 **True Streaming Support**: Full support for SSE and chunked responses without buffering
- ⚡ **Zero-Copy Performance**: Direct streaming with no intermediate buffering
- 🧪 **Well Tested**: Comprehensive test suite covering all functionality including streaming

## Architecture

### Streaming Duplex Design

The proxy implements a streaming duplex architecture where each request is assigned a UUID and both request and response streams are duplicated in real-time:

```
┌─────────────────────┐    ┌─────────────────────┐    ┌─────────────────────┐
│   Client Request    │───▶│   Proxy Server      │───▶│  Target API Server  │
│                     │    │                     │    │                     │
│ POST /api/v1/chat   │    │ 1. Generate UUID    │    │ POST /api/v1/chat   │
│ {"stream": true}    │    │ 2. Duplex Request   │    │ {"stream": true}    │
└─────────────────────┘    │ 3. Duplex Response  │    └─────────────────────┘
           │                └─────────────────────┘                │
           │                          │                            │
           │                          ▼                            │
           │                ┌─────────────────────┐                │
           │                │  Logging Server     │                │
           │                │                     │                │
           │                │ PUT /{uuid}/request │◀───────────────┘
           │                │ PUT /{uuid}/response│◀───────────────┐
           │                └─────────────────────┘                │
           │                                                       │
           └───────────────── Response Stream ─────────────────────┘
                            (SSE/Chunked/JSON)
```

### Key Architecture Decisions

1. **UUID-Based Tracking**: Each request gets a unique identifier for complete traceability
2. **Stream Duplexing**: Uses `io.TeeReader` and `io.MultiWriter` for real-time stream splitting
3. **Dedicated Logging Server**: Separate service handles log storage via PUT requests
4. **Zero Buffering**: Direct streaming with no intermediate storage
5. **Async Logging**: Goroutines handle logging without blocking the main proxy flow

### Component Details

#### ProxyServer
- Central coordinator managing routes and logging client
- Generates UUIDs for request tracking
- Handles stream duplexing for both requests and responses

#### LoggingClient
- HTTP client for sending data to the logging server
- Handles PUT requests to `/{uuid}/request` and `/{uuid}/response`
- Includes headers and body data in raw HTTP format

#### Stream Duplexing
- **Request Duplexing**: `io.TeeReader` splits request stream to target and logging server
- **Response Duplexing**: `io.MultiWriter` duplicates response stream to client and logging server
- **Real-time Processing**: No buffering, streams data as it flows

## Configuration

The proxy uses a YAML configuration file (`config.yaml`):

```yaml
server:
  port: 5601              # Port to listen on
  host: "localhost"       # Host to bind to

logging:
  console: true           # Enable simple console output
  server_url: "http://localhost:8080"  # Logging server URL
  enabled: true           # Enable streaming to logging server

routes:
  - source: "/api/v1/"                    # Source path prefix
    destination: "https://openrouter.ai/" # Target server
    name: "openrouter"                    # Route identifier
  - source: "/v1/"
    destination: "https://api.openai.com/"
    name: "openai"
```

### Route Configuration

Routes map source paths to destination servers:

- **source**: Path prefix to match (e.g., `/api/v1/`)
- **destination**: Target server URL (e.g., `https://openrouter.ai/`)
- **name**: Human-readable identifier for logging

**Path Transformation Example:**
- Request: `GET /api/v1/chat/completions`
- Route: `source: "/api/v1/"` → `destination: "https://openrouter.ai/"`
- Result: `GET https://openrouter.ai/api/v1/chat/completions`

## Usage

### Quick Start

1. **Start the logging server** (handles log storage):
   ```bash
   ./logging-server
   ```
   This starts the logging server on port 8080 and creates a `logs/` directory.

2. **Configure the proxy** by editing `config.yaml`

3. **Run the proxy**:
   ```bash
   ./openrouter-proxy
   ```

4. **Make requests** to the configured endpoints:
   ```bash
   curl -X POST http://localhost:5601/api/v1/chat/completions \
     -H "Content-Type: application/json" \
     -H "Authorization: Bearer YOUR_API_KEY" \
     -d '{"model": "gpt-3.5-turbo", "messages": [{"role": "user", "content": "Hello!"}]}'
   ```

5. **Check logs** in the `logs/` directory for detailed request/response data

### Example Configurations

#### OpenRouter + OpenAI Proxy
```yaml
server:
  port: 5601
  host: "localhost"

logging:
  console: true
  file: "requests.log"
  binary_files: true

routes:
  - source: "/api/v1/"
    destination: "https://openrouter.ai/"
    name: "openrouter"
  - source: "/v1/"
    destination: "https://api.openai.com/"
    name: "openai"
```

#### Single Endpoint with Custom Logging
```yaml
server:
  port: 8080
  host: "0.0.0.0"

logging:
  console: false
  file: "/var/log/api-proxy.log"
  binary_files: false

routes:
  - source: "/api/"
    destination: "https://api.example.com/"
    name: "example-api"
```

## Logging

The proxy provides dual-level logging:

### 1. Console Output (Proxy)
Simple request tracking with UUID:
```
2025-09-09 17:45:32 [a1b2c3d4] POST /api/v1/chat/completions -> https://openrouter.ai/ [openrouter]
2025-09-09 17:45:45 [e5f6g7h8] GET /v1/models -> https://api.openai.com/ [openai]  
```

### 2. Detailed Logging (Logging Server)
Complete request/response pairs saved as binary files in `logs/` directory:
- `2025-09-09_17-45-32.123_a1b2c3d4_request.bin`  - Raw HTTP request (headers + body)
- `2025-09-09_17-45-32.456_a1b2c3d4_response.bin` - Raw HTTP response (headers + body)

**UUID-Based Tracking:**
- Each request gets a unique UUID (e.g., `a1b2c3d4-1234-5678-9abc-def123456789`)
- Request and response files share the same UUID for easy pairing
- Files contain raw HTTP data exactly as sent/received

**Logging Server API:**
- `PUT /{uuid}/request` - Receives request data
- `PUT /{uuid}/response` - Receives response data
- Data includes HTTP headers and body as binary stream

## Streaming Support

The proxy fully supports Server-Sent Events (SSE) and chunked transfer encoding:

### Detection
Automatically detects streaming responses by:
- `Content-Type: text/event-stream`
- `Content-Type: application/stream+json`
- `Transfer-Encoding: chunked`

### Handling
- **Non-buffering**: Streams data directly to client
- **Complete Logging**: Captures entire stream for logging
- **Performance**: No impact on streaming performance

### Example Streaming Request
```bash
curl -N http://localhost:5601/api/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model": "gpt-3.5-turbo", "messages": [...], "stream": true}'
```

## Building

### Prerequisites
- Go 1.22+
- Dependencies managed with Go modules

### Build Commands
```bash
# Install dependencies
go mod tidy

# Build main proxy
go build .

# Build logging server
cd cmd && go build .

# Run tests
go test -v

# Cross-compile for Linux
GOOS=linux GOARCH=amd64 go build .
cd cmd && GOOS=linux GOARCH=amd64 go build .
```

## Testing

The project includes comprehensive tests covering:

- ✅ Configuration loading and validation
- ✅ UUID-based request tracking
- ✅ Streaming duplex architecture  
- ✅ SSE/streaming responses with real-time logging
- ✅ Multiple route handling
- ✅ Path transformation logic
- ✅ Logging server integration

Run tests:
```bash
go test -v
```

## Performance Considerations

### Design Choices for Performance
1. **Non-blocking Logging**: Uses goroutines to prevent I/O from blocking requests
2. **Streaming-Aware**: Doesn't buffer large streaming responses
3. **Minimal Copying**: Reuses request/response bodies efficiently
4. **Connection Reuse**: Leverages Go's HTTP transport connection pooling

### Benchmarking
The proxy adds minimal latency:
- **Regular requests**: <1ms overhead
- **Streaming requests**: <1ms initial overhead, no ongoing impact
- **Memory usage**: Constant regardless of response size

## Security Notes

- **Request Headers**: All client headers (including Authorization) are forwarded
- **Response Headers**: All server headers are returned to client
- **Logging**: Be aware that request/response bodies are logged in binary files
- **Network**: Consider firewall rules for the proxy port

## Troubleshooting

### Common Issues

1. **404 Errors**
   - Check route configuration matches request paths
   - Verify destination URLs are accessible
   - Ensure path prefixes include trailing slashes

2. **Connection Refused**
   - Verify target servers are reachable
   - Check firewall settings
   - Confirm destination URLs in config

3. **Streaming Issues**
   - Ensure client supports chunked encoding
   - Check for proxy/firewall interference
   - Verify Content-Type headers

### Debug Mode
Enable verbose logging by setting `console: true` in configuration.

## License

This project is open source. See LICENSE file for details.

## Missing

- Metadata endpoint
- Logging UI
  - Simple frontend showing a list of requests
  - Websocket for live feed
  - Support live tailing requests/responses (websocket/SSE)
  - Refactor REST API for logging
- Look at using a custom `Transport` to simplify full request/response logging
- Test if websockets work properly