#!/bin/sh
set -eu

hostport="${OLLAMA_HOST:-127.0.0.1:11434}"
export OLLAMA_HOST="$hostport"

echo "Starting Ollama server at $OLLAMA_HOST"
exec ollama serve
