#!/bin/bash
curl -X POST http://localhost:5601/lmstudio/v1/chat/completions \
  -H "Authorization: Bearer YOUR_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "liquid/lfm2-1.2b",
    "messages": [
      {
        "role": "user",
        "content": "How does streaming with an OpenAI /v1/chat/completions endpoint work?"
      }
    ],
    "stream": true
  }' \
  --no-buffer