#!/bin/sh
set -eu

base_url="${OLLAMA_BASE_URL:-http://127.0.0.1:11434}"
model="${OLLAMA_MODEL:-qwen3:1.7b}"

hostport="${base_url#http://}"
hostport="${hostport#https://}"
export OLLAMA_HOST="$hostport"

echo "Waiting for Ollama API at $base_url"
for i in $(seq 1 60); do
  if ollama list >/dev/null 2>&1; then
    break
  fi
  sleep 2
done

echo "Pulling model: $model"
ollama pull "$model"

echo "Ollama model ready: $model"
