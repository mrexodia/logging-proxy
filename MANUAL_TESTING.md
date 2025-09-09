# Manual Testing Guide

This guide provides step-by-step instructions for manually testing the OpenRouter Proxy to verify all functionality works correctly.

## Prerequisites

- Go 1.22+ installed
- Valid API keys for OpenRouter and/or OpenAI
- curl or similar HTTP client
- Two terminal windows

## Build and Setup

### 1. Build Executables

```bash
# Build main proxy
go build -o openrouter-proxy.exe .

# Build logging server
cd cmd && go build -o ../logging-server.exe .
cd ..
```

### 2. Start Services

**Terminal 1 - Logging Server:**
```bash
./logging-server.exe
```
You should see:
```
Logging server starting on :8080
Saving logs to ./logs/ directory
```

**Terminal 2 - Proxy Server:**
```bash
./openrouter-proxy.exe
```
You should see:
```
Route: /api/v1/ -> https://openrouter.ai/ (openrouter)
Route: /v1/ -> https://api.openai.com/ (openai)
Proxy server starting on localhost:5601
Logging server: http://localhost:8080
```

## Basic Functionality Tests

### Test 1: OpenRouter Route
```bash
curl -X POST http://localhost:5601/api/v1/models \
  -H "Authorization: Bearer YOUR_OPENROUTER_KEY" \
  -H "Content-Type: application/json"
```

**Expected:**
- Console shows: `[UUID] POST /api/v1/models -> https://openrouter.ai/ [openrouter]`
- Response contains OpenRouter model list
- New files appear in `logs/` directory

### Test 2: OpenAI Route
```bash
curl -X POST http://localhost:5601/v1/models \
  -H "Authorization: Bearer YOUR_OPENAI_KEY" \
  -H "Content-Type: application/json"
```

**Expected:**
- Console shows: `[UUID] POST /v1/models -> https://api.openai.com/ [openai]`
- Response contains OpenAI model list
- New log files created with different UUID

### Test 3: 404 Handling
```bash
curl -X GET http://localhost:5601/invalid/path
```

**Expected:**
- HTTP 404 response
- No log files created (no matching route)

## Streaming Tests

### Test 4: Streaming Chat Completion
```bash
curl -N http://localhost:5601/api/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer YOUR_OPENROUTER_KEY" \
  -d '{
    "model": "openai/gpt-3.5-turbo",
    "messages": [{"role": "user", "content": "Count to 5 slowly, one number per response"}],
    "stream": true
  }'
```

**Expected:**
- Real-time streaming response with `data:` chunks
- Console logs the request immediately
- Complete request/response logged to files after stream ends

### Test 5: Large Response Streaming
```bash
curl -N http://localhost:5601/api/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer YOUR_OPENROUTER_KEY" \
  -d '{
    "model": "openai/gpt-3.5-turbo",
    "messages": [{"role": "user", "content": "Write a 500-word story about a robot"}],
    "stream": true
  }'
```

**Expected:**
- Streaming response begins immediately
- No buffering delays
- Complete story logged to response file

## Concurrency Tests

### Test 6: Multiple Simultaneous Requests
```bash
# Run these commands simultaneously (in separate terminals or with &)
curl -X POST http://localhost:5601/api/v1/models -H "Authorization: Bearer KEY1" &
curl -X POST http://localhost:5601/api/v1/models -H "Authorization: Bearer KEY2" &
curl -X POST http://localhost:5601/v1/models -H "Authorization: Bearer KEY3" &
wait
```

**Expected:**
- All requests complete successfully
- Each gets unique UUID in console logs
- Separate log files for each request
- No interference between requests

### Test 7: Concurrent Streaming
```bash
# Start multiple streaming requests
curl -N http://localhost:5601/api/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer KEY" \
  -d '{"model": "openai/gpt-3.5-turbo", "messages": [{"role": "user", "content": "Count to 10"}], "stream": true}' &

curl -N http://localhost:5601/api/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer KEY" \
  -d '{"model": "openai/gpt-3.5-turbo", "messages": [{"role": "user", "content": "List 5 animals"}], "stream": true}' &

wait
```

**Expected:**
- Both streams start immediately
- No cross-contamination between streams
- Each stream completes independently
- Separate UUID tracking for each

## Stress Testing

### Test 8: High Volume Requests
```bash
# Using Apache Bench (install with: apt-get install apache2-utils)
ab -n 100 -c 10 -H "Authorization: Bearer YOUR_KEY" \
  -p request.json -T "application/json" \
  http://localhost:5601/api/v1/models

# Or using hey (install from: https://github.com/rakyll/hey)
hey -n 100 -c 10 -H "Authorization: Bearer YOUR_KEY" \
  http://localhost:5601/api/v1/models
```

Create `request.json`:
```json
{
  "model": "openai/gpt-3.5-turbo",
  "messages": [{"role": "user", "content": "Hello"}]
}
```

**Expected:**
- All 100 requests complete successfully
- Response times remain consistent
- No memory leaks or performance degradation
- 100 unique UUIDs in logs

## Logging Verification

### Test 9: Log File Inspection
After running tests, check the logs:

```bash
ls -la logs/
```

**Expected structure:**
```
2025-09-09_17-45-32.123_a1b2c3d4_request.bin
2025-09-09_17-45-32.456_a1b2c3d4_response.bin
2025-09-09_17-46-15.789_e5f6g7h8_request.bin
2025-09-09_17-46-15.890_e5f6g7h8_response.bin
```

### Test 10: Log Content Verification
```bash
# Check request log content (should show HTTP headers + body)
head -c 500 logs/*_request.bin

# Check response log content (should show HTTP headers + body)  
head -c 500 logs/*_response.bin
```

**Expected:**
- Request files contain HTTP headers and request body
- Response files contain HTTP headers and response body
- UUIDs match between request/response pairs

## Error Handling Tests

### Test 11: Invalid Target Server
Temporarily edit `config.yaml` to point to an invalid destination:
```yaml
routes:
  - source: "/test/"
    destination: "https://invalid.nonexistent.domain/"
    name: "invalid"
```

```bash
curl -X POST http://localhost:5601/test/endpoint
```

**Expected:**
- HTTP 502 Bad Gateway response
- Error logged in proxy console
- Request logged but no response file (failed connection)

### Test 12: Logging Server Down
Stop the logging server and make requests:

```bash
curl -X POST http://localhost:5601/api/v1/models \
  -H "Authorization: Bearer YOUR_KEY"
```

**Expected:**
- Request still proxied successfully to target
- Error logged about logging server connection
- Response returned to client normally (logging failure doesn't break proxy)

## Performance Expectations

### Typical Performance Metrics
- **Request overhead**: <1ms additional latency
- **Memory usage**: Constant (no buffering)
- **Concurrent requests**: 1000+ simultaneous connections
- **Streaming**: Real-time with no buffering delays

### Monitoring During Tests
Watch for:
- Response times remain low
- Memory usage stays constant
- No goroutine leaks
- Log files created promptly

## Troubleshooting

### Common Issues

**404 Errors:**
- Check route configuration in `config.yaml`
- Verify path prefixes include trailing slashes
- Confirm destination URLs are accessible

**Connection Refused:**
- Verify target API servers are reachable
- Check API keys are valid
- Confirm firewall isn't blocking requests

**Streaming Not Working:**
- Use `-N` flag with curl for streaming
- Check for proxy/firewall interference
- Verify Content-Type headers

**Missing Log Files:**
- Confirm logging server is running
- Check `logs/` directory permissions
- Verify logging is enabled in config

### Debug Mode
Enable verbose logging by setting `console: true` in `config.yaml` to see detailed request tracking.

## Go HTTP Server Concurrency

The proxy leverages Go's built-in HTTP server concurrency model:

- **Automatic goroutines**: Each request gets its own goroutine
- **Non-blocking**: Slow requests don't affect others
- **Connection pooling**: Efficient reuse of connections to target servers
- **Memory efficient**: Lightweight goroutines (2KB initial stack)

This architecture allows the proxy to handle thousands of concurrent requests without performance degradation.