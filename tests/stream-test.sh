#!/bin/bash
curl -X POST http://localhost:5601/lmstudio/v1/chat/completions \
  -H "Authorization: Bearer YOUR_API_KEY" \
  -H "Content-Type: application/json" \
  -d @stream-test.json \
  --no-buffer