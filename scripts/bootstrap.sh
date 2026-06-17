#!/bin/sh
set -eu

mkdir -p "${CONFIG_DIR:-/llama_lab/config}" "${LOG_DIR:-/llama_lab/logs}" "${STATE_DIR:-/llama_lab/state}"

if [ ! -f "${CONFIG_DIR:-/llama_lab/config}/admin.env" ]; then
  user="${BOOTSTRAP_ADMIN_USER:-admin}"
  pass="${BOOTSTRAP_ADMIN_PASS:-ChangeMeNow123!}"
  pass_hash="$(PASS="$pass" node -e '
const crypto = require("crypto");
const pass = process.env.PASS || "";
const N = 16384;
const r = 8;
const p = 1;
const keyLen = 32;
const salt = crypto.randomBytes(16);
const hash = crypto.scryptSync(pass, salt, keyLen, { N, r, p, maxmem: 64 * 1024 * 1024 });
process.stdout.write(`scrypt$${N}$${r}$${p}$${salt.toString("base64")}$${hash.toString("base64")}`);
')"
  {
    echo "ADMIN_USER=${user}"
    echo "ADMIN_PASS_HASH=${pass_hash}"
    echo "MUST_CHANGE_PASSWORD=true"
  } >"${CONFIG_DIR:-/llama_lab/config}/admin.env"
  chmod 600 "${CONFIG_DIR:-/llama_lab/config}/admin.env"
  echo "Generated first-run admin credentials in config volume"
  echo "Username: ${user}"
  echo "Password: ${pass}"
  echo "Password change is required on first login"
fi
