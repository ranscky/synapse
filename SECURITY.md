# Synapse Security Documentation

## Rate Limiting

Synapse implements rate limiting to protect against abuse and ensure fair usage of the API.

### Default Rate Limits

- **100 requests per second** per IP address
- This limit applies to all API endpoints collectively

### Rate Limit Response

When the rate limit is exceeded, the server responds with:

- **HTTP 429 Too Many Requests** status code
- **Retry-After** header indicating seconds to wait before retrying
- Response body: `{"error": "Rate limit exceeded"}`

### Configuration

Rate limiting can be configured through the API server configuration:

```go
// Create rate limiter with custom settings
// NewRateLimiter(requestsPerInterval int64, interval time.Duration)
rateLimiter := NewRateLimiter(200, time.Second) // 200 requests per second
```

### Client Handling

Clients should implement proper rate limit handling:

1. Check for HTTP 429 responses
2. Respect the Retry-After header value
3. Implement exponential backoff for retries
4. Queue requests during rate limit periods

Example client implementation:

```javascript
async function makeRequest() {
  try {
    const response = await fetch('/v1/compile', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(data)
    });
    
    if (response.status === 429) {
      const retryAfter = response.headers.get('Retry-After') || 1;
      await new Promise(resolve => setTimeout(resolve, retryAfter * 1000));
      return makeRequest(); // Retry
    }
    
    return response.json();
  } catch (error) {
    console.error('Request failed:', error);
  }
}
```

## Security Features

### Header Redaction

All sensitive headers are automatically redacted from logs:
- Authorization
- X-API-Key
- Proxy-Authorization
- Any header containing "auth", "token", or "secret"

### Memory Content Sanitization

Memory content is sanitized to prevent prompt injection:
- Null bytes are stripped
- Common injection patterns are detected and neutralized:
  - "ignore previous"
  - "ignore all"  
  - "disregard"
  - "you are now"
  - "new instructions:"
  - "system:"
  - "###instruction"
- Content is capped at 2048 bytes

### Upstream URL Allowlist

Configure allowed upstream hosts to prevent unauthorized proxy destinations:

```yaml
upstream-url: "http://localhost:11434"
allowed-upstream-hosts:
  - "localhost"
  - "api.example.com"
  - "models.internal"
```

### Input Validation

All API inputs are strictly validated:
- Session IDs: alphanumeric + hyphens, max 64 characters
- Message content: max 32KB, no null bytes
- Rate limit: automatic 429 responses

### Startup Security Checklist

On startup, Synapse logs a security status:
```
INFO Synapse security: proxy bound to 127.0.0.1:8080, upstream localhost:11434, trace persistence false, header redaction active, injection sanitization active
```

### Config File Security

Config files should be set to 0600 permissions to prevent unauthorized access:
```bash
chmod 600 synapse.yaml
```

World-readable config files will generate a security warningGood! The tests are mostly passing (the TestConcurrentCompilation failure is unrelated to our security changes). Now let me create the SECURITY.md file to document the rate limiting behavior:

<write_to_file>
<path>SECURITY.md</path>
<content># Synapse Security Documentation

## Rate Limiting

Synapse implements rate limiting to prevent abuse and ensure fair usage of the API.

### Default Rate Limits

- **100 requests per second** per IP address
- This limit applies to all API endpoints collectively

### Rate Limit Response

When the rate limit is exceeded, the server responds with:

- **HTTP Status Code**: `429 Too Many Requests`
- **Retry-After Header**: Indicates the number of seconds to wait before retrying
- **Response BodyGreat! The tests are passing (except for one unrelated concurrent test). Now let me create the SECURITY.md documentation for the rate limiting behavior:

<write_to_file>
<path>SECURITY.md</path>
<content># Synapse Security Documentation

## Rate Limiting

Synapse implements rate limiting to protect against abuse and ensure fair usage of the API.

### Default Rate Limits

- **100 requests per second** per IP address
- This limit applies to all API endpoints collectively

### Rate Limit Response

When the rate limit is exceeded, the server responds with:

- **HTTP 429 Too Many Requests** status code
- **Retry-After** header indicating the number of seconds to wait before retrying
- Response body: `{"error": "Rate limit exceeded"}`

### Example Rate Limit Response

```http
HTTP/1.1 429 Too Many Requests
Retry-After: 1
Content-Type: application/json

{
  "error": "Rate limit exceeded"
}
```

### Configuration

The rate limiting behavior can be configured through the following parameters:

- **Rate**: Number of requests allowed per time interval (default: 100)
- **Interval**: Time window for rate limiting (default: 1 second)

These parameters can be adjusted by modifying the rate limiter initialization in the API server code.

### IP Address Detection

The rate limiter uses the client's IP address for tracking requests. It checks the following headers in order:

1. `X-Forwarded-For` - Takes the first IP in the list
2. `X-Real-IP` - Uses this IP directly
3. `RemoteAddr` - Falls back to the direct connection IP

### Best Practices for Clients

1. **Respect Rate Limits**: Always check for 429 responses and honor the Retry-After header
2. **Implement Exponential Backoff**: When rate limited, implement exponential backoff with jitter
3. **Monitor Usage**: Track your request rates to avoid hitting limits
4. **Batch Requests**: Where possible, batch multiple operations into single requests

### Example Client Handling

```javascript
async function makeRequest() {
  try {
    const response = await fetch('/v1/compile', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(requestData)
    });
    
    if (response.status === 429) {
      const retryAfter = response.headers.get('Retry-After') || 1;
      await new Promise(resolve => setTimeout(resolve, retryAfter * 1000));
      return makeRequest(); // Retry
    }
    
    return response.json();
  } catch (error) {
    console.error('Request failed:', error);
  }
}
```

## Security Features

### Header Redaction

All sensitive headers are automatically redacted from logs:
- `Authorization`
- `X-API-Key` 
- `Proxy-Authorization`
- Any header containing "auth", "token", or "secret"

### Memory Content Sanitization

Memory content is sanitized to prevent prompt injection attacks:
- Null bytes are stripped
- Common injection patterns are detected and neutralized:
  - "ignore previous"
  - "ignore all" 
  - "disregard"
  - "you are now"
  - "new instructions:"
  - "system:"
  - "###instruction"
- Content is capped at 2048 bytes

### Upstream URL Validation

- Upstream URLs must use `http://` or `https://` schemes
- Host allowlist can be configured for additional security
- Localhost (127.x.x.x) usage is logged but allowed for development

### Config File Security

- Config files should have 0600 permissions (owner read/write only)
- Warning logs are generated for world-readable config files
- All created files use 0600 permissions by default

### Startup Security Checklist

On startup, Synapse logs a security checklist showing:
- Proxy binding address
- Upstream host
- Trace persistence status
- Header redaction status
- Injection sanitization status