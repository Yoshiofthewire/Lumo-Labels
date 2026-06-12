#!/bin/sh
set -eu

mkdir -p /lumo_lab/config /lumo_lab/logs /lumo_lab/state
chown -R lumolab:lumolab /lumo_lab

exec supervisord -c /etc/supervisord.conf