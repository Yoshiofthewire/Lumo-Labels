#!/bin/sh
set -eu

mkdir -p /llama_lab/config /llama_lab/logs /llama_lab/state
chown -R llamalab:llamalab /llama_lab

exec supervisord -c /etc/supervisord.conf