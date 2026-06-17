#!/bin/sh
set -eu

LLAMA_DIR="${LLAMA_LOCAL_DIR:-/opt/llama-api-v2}"
AUTH_FILE="${LLAMA_AUTH_FILE:-/llama_lab/config/llama-auth.json}"

if [ "${LLAMA_LOCAL_ENABLED:-true}" != "true" ]; then
  echo "Local Llama process disabled by LLAMA_LOCAL_ENABLED"
  exec tail -f /dev/null
fi

if [ ! -d "$LLAMA_DIR" ]; then
  echo "Llama directory not found: $LLAMA_DIR"
  exec tail -f /dev/null
fi

cd "$LLAMA_DIR"

# Wait for auth file to be provided before starting Llama
while [ ! -f "$AUTH_FILE" ]; do
  echo "Waiting for Llama auth file at $AUTH_FILE ..."
  sleep 10
done

cp "$AUTH_FILE" "$LLAMA_DIR/auth.json"

kill_stale_llama() {
  for attempt in 1 2 3; do
    pids=$(lsof -ti:3333 2>/dev/null || true)
    if [ -z "$pids" ]; then
      break
    fi
    echo "Killing stale process on port 3333 (attempt $attempt): $pids"
    echo "$pids" | xargs -r kill -9 2>/dev/null || true
    sleep 1
  done
  sleep 2
  lsof -ti:3333 2>/dev/null && echo "WARNING: Port 3333 still in use after cleanup attempts" || true
}

kill_stale_llama

if ! node llama.js ghost:true; then
  echo "Llama process exited unexpectedly; keeping container alive for recovery."
  exec tail -f /dev/null
fi
