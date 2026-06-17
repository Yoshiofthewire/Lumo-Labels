#!/bin/sh
set -eu

hostport="${OLLAMA_HOST:-127.0.0.1:11434}"
export OLLAMA_HOST="$hostport"
export OLLAMA_MODELS="${OLLAMA_MODELS:-/llama_lab/ollama-models}"

mkdir -p "$OLLAMA_MODELS"

echo "Starting Ollama server at $OLLAMA_HOST"
echo "Using Ollama models dir: $OLLAMA_MODELS"
exec ollama serve
