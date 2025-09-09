#!/usr/bin/env python3
"""
OpenRouter Proxy Usage Example

This script demonstrates how to use the OpenRouter Proxy for various API calls
including streaming and non-streaming requests to both OpenRouter and OpenAI endpoints.

Usage:
    python example.py

Requirements:
    pip install requests
"""

import json
import os
import requests
import time
from typing import Iterator, Dict, Any

# Proxy configuration
PROXY_BASE_URL = "http://localhost:5601"

# API Keys (set these as environment variables)
OPENROUTER_API_KEY = os.getenv("OPENROUTER_API_KEY", "your-openrouter-key-here")
OPENAI_API_KEY = os.getenv("OPENAI_API_KEY", "your-openai-key-here")


def make_request(endpoint: str, data: Dict[Any, Any], api_key: str, stream: bool = False) -> requests.Response:
    """Make a request through the proxy."""
    headers = {
        "Content-Type": "application/json",
        "Authorization": f"Bearer {api_key}"
    }
    
    url = f"{PROXY_BASE_URL}{endpoint}"
    
    print(f"→ Making request to: {endpoint}")
    print(f"  Stream: {stream}")
    
    response = requests.post(url, headers=headers, json=data, stream=stream)
    return response


def test_openrouter_models():
    """Test listing OpenRouter models."""
    print("\n=== Testing OpenRouter Models ===")
    
    try:
        response = requests.get(
            f"{PROXY_BASE_URL}/api/v1/models",
            headers={"Authorization": f"Bearer {OPENROUTER_API_KEY}"}
        )
        
        if response.status_code == 200:
            models = response.json()
            print(f"✓ Found {len(models.get('data', []))} OpenRouter models")
            # Show first few models
            for model in models.get('data', [])[:3]:
                print(f"  - {model.get('id', 'unknown')}")
        else:
            print(f"✗ Error: {response.status_code} - {response.text}")
            
    except Exception as e:
        print(f"✗ Exception: {e}")


def test_openai_models():
    """Test listing OpenAI models."""
    print("\n=== Testing OpenAI Models ===")
    
    try:
        response = requests.get(
            f"{PROXY_BASE_URL}/v1/models",
            headers={"Authorization": f"Bearer {OPENAI_API_KEY}"}
        )
        
        if response.status_code == 200:
            models = response.json()
            print(f"✓ Found {len(models.get('data', []))} OpenAI models")
            # Show first few models
            for model in models.get('data', [])[:3]:
                print(f"  - {model.get('id', 'unknown')}")
        else:
            print(f"✗ Error: {response.status_code} - {response.text}")
            
    except Exception as e:
        print(f"✗ Exception: {e}")


def test_chat_completion(streaming: bool = False):
    """Test chat completion through OpenRouter."""
    print(f"\n=== Testing Chat Completion (Stream: {streaming}) ===")
    
    data = {
        "model": "openai/gpt-3.5-turbo",
        "messages": [
            {"role": "user", "content": "Say hello and count to 3"}
        ],
        "stream": streaming,
        "max_tokens": 100
    }
    
    try:
        response = make_request("/api/v1/chat/completions", data, OPENROUTER_API_KEY, stream=streaming)
        
        if streaming:
            print("✓ Streaming response:")
            content = ""
            for line in response.iter_lines():
                if line:
                    line_str = line.decode('utf-8')
                    if line_str.startswith('data: '):
                        data_str = line_str[6:]
                        if data_str.strip() == '[DONE]':
                            break
                        try:
                            chunk = json.loads(data_str)
                            delta = chunk.get('choices', [{}])[0].get('delta', {})
                            if 'content' in delta:
                                content += delta['content']
                                print(delta['content'], end='', flush=True)
                        except json.JSONDecodeError:
                            continue
            print(f"\n  Full response: {content}")
        else:
            if response.status_code == 200:
                result = response.json()
                message = result.get('choices', [{}])[0].get('message', {}).get('content', '')
                print(f"✓ Response: {message}")
            else:
                print(f"✗ Error: {response.status_code} - {response.text}")
                
    except Exception as e:
        print(f"✗ Exception: {e}")


def test_concurrent_requests():
    """Test multiple concurrent requests."""
    print("\n=== Testing Concurrent Requests ===")
    
    import concurrent.futures
    import threading
    
    def make_test_request(request_id: int) -> str:
        """Make a single test request."""
        data = {
            "model": "openai/gpt-3.5-turbo",
            "messages": [
                {"role": "user", "content": f"This is request #{request_id}. Just say 'Hello {request_id}'"}
            ],
            "max_tokens": 20
        }
        
        try:
            response = make_request("/api/v1/chat/completions", data, OPENROUTER_API_KEY)
            if response.status_code == 200:
                result = response.json()
                message = result.get('choices', [{}])[0].get('message', {}).get('content', '')
                return f"Request {request_id}: {message.strip()}"
            else:
                return f"Request {request_id}: Error {response.status_code}"
        except Exception as e:
            return f"Request {request_id}: Exception {e}"
    
    # Execute 5 concurrent requests
    print("Making 5 concurrent requests...")
    start_time = time.time()
    
    with concurrent.futures.ThreadPoolExecutor(max_workers=5) as executor:
        futures = [executor.submit(make_test_request, i) for i in range(1, 6)]
        results = [future.result() for future in concurrent.futures.as_completed(futures)]
    
    end_time = time.time()
    
    print("✓ Concurrent request results:")
    for result in results:
        print(f"  {result}")
    
    print(f"  Total time: {end_time - start_time:.2f} seconds")


def test_error_handling():
    """Test error handling with invalid requests."""
    print("\n=== Testing Error Handling ===")
    
    # Test invalid endpoint
    try:
        response = requests.get(f"{PROXY_BASE_URL}/invalid/endpoint")
        print(f"Invalid endpoint: {response.status_code} (expected 404)")
    except Exception as e:
        print(f"Invalid endpoint exception: {e}")
    
    # Test invalid API key
    try:
        response = requests.get(
            f"{PROXY_BASE_URL}/api/v1/models",
            headers={"Authorization": "Bearer invalid-key"}
        )
        print(f"Invalid API key: {response.status_code} (expected 401/403)")
    except Exception as e:
        print(f"Invalid API key exception: {e}")


def main():
    """Run all test examples."""
    print("OpenRouter Proxy Example Script")
    print("================================")
    print(f"Proxy URL: {PROXY_BASE_URL}")
    print(f"OpenRouter API Key: {'Set' if OPENROUTER_API_KEY != 'your-openrouter-key-here' else 'Not set'}")
    print(f"OpenAI API Key: {'Set' if OPENAI_API_KEY != 'your-openai-key-here' else 'Not set'}")
    
    # Check if proxy is running
    try:
        response = requests.get(f"{PROXY_BASE_URL}/invalid", timeout=5)
        print("✓ Proxy server is responding")
    except requests.exceptions.RequestException:
        print("✗ Proxy server is not running. Start with: ./openrouter-proxy.exe")
        return
    
    # Run tests
    test_openrouter_models()
    test_openai_models()
    test_chat_completion(streaming=False)
    test_chat_completion(streaming=True)
    test_concurrent_requests()
    test_error_handling()
    
    print("\n=== Testing Complete ===")
    print("Check the logs/ directory for detailed request/response data.")


if __name__ == "__main__":
    main()