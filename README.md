# Lumo Labels: Proton Mail Auto-Labeler

Lumo Labels is a Dockerized application that scans unread Proton Mail inbox messages, classifies them with Lumo API V2, and applies Proton labels using deterministic rules.

Classification prompts are composed in this order:

1. `GARDRAIL.md`
2. `TUNING.md`
3. Message-specific classification prompt (sender, subject, body, allowlist)

This repository currently includes:

- Go backend daemon + API server
- React web interface
- Single-container runtime with `supervisord`
- Local Lumo API V2 process support inside the same container

## Overview

The app runs as a long-lived daemon and polls Proton Mail every 5 minutes by default.

For each eligible message:

1. Fetch unread Inbox messages only
2. Convert body to plain text and redact configured sensitive patterns
3. Send classification prompt to Lumo
4. Parse Lumo output using configured Proton allowlist
5. Apply matching label to Proton message
6. Persist state/checkpoint to avoid re-processing

## Architecture

- Backend (`backend/`)
: Go service with daemon and HTTP API modes
- Frontend (`frontend/`)
: React + Vite web UI
- Runtime
: Docker container with three supervised processes:
  - API server (`lumo-lab --mode server`)
  - Poll daemon (`lumo-lab --mode daemon`)
  - Local Lumo API V2 (`node lumo.js`)

## Key Features Implemented

- Local auth with session cookie middleware
- First-run admin bootstrap (`admin.env`)
- Config/state persisted in mounted volumes
- 30-day rolling cleanup for processed IDs and decision history
- Console + rotating log files (16 MB, keep 8)
- Health endpoint and repair endpoint
- Automatic unhealthy restart escalation support
- Lumo settings configurable through web config API/UI
- Lumo connectivity test endpoint + UI test button
- Editable `TUNING.md` from the web UI
- Proton label discovery exposed to the UI for tuning order/definitions

## Project Structure

- `backend/cmd/main.go`
- `backend/internal/app/`
- `backend/internal/api/`
- `backend/internal/adapters/proton/`
- `backend/internal/adapters/lumo/`
- `backend/internal/processor/`
- `backend/internal/redaction/`
- `backend/internal/state/`
- `frontend/src/`
- `Dockerfile`
- `docker-compose.yml`
- `supervisord.conf`
- `scripts/bootstrap.sh`
- `scripts/start-lumo.sh`

## Requirements

- Docker + Docker Compose

Authentication setup guide:

- [AuthHowTo.md](AuthHowTo.md)

Optional for local non-Docker development:

- Go 1.22+
- Node.js 20+
- npm

## Quick Start (Docker)

1. Create environment file:

```bash
cp .env.example .env
```

2. Edit `.env` values:

- `PROTON_AUTH_FILE` (default: `/lumo_lab/config/proton-auth.json`)
- `PROTON_APP_VERSION` (optional override; default in app code is `web-mail@5.0.0.0`)
- `LUMO_API_KEY` if your Lumo route requires it
- `LUMO_BASE_URL` (defaults to local in-container Lumo)

3. Build and run:

```bash
docker compose up --build -d
```

4. Open the app:

- Web UI: http://localhost:5866

5. First login credentials:

- Username: `admin`
- Password: `ChangeMeNow123!`
- You will be required to change the password on first login.

Optional bootstrap override values in `.env`:

- `BOOTSTRAP_ADMIN_USER` (default: `admin`)
- `BOOTSTRAP_ADMIN_PASS` (default: `ChangeMeNow123!`)

## Volumes and Persistence

The container persists data via named volumes mapped to:

- `/lumo_lab/config`
- `/lumo_lab/logs`
- `/lumo_lab/state`

Important files:

- `/lumo_lab/config/config.yaml`
- `/lumo_lab/config/admin.env`
- `/lumo_lab/config/lumo-auth.json` (for local Lumo API V2 session)
- `/lumo_lab/config/proton-auth.json` (generated Proton API token artifact)
- `/lumo_lab/config/TUNING.md` (runtime tuning instructions; created/updated by API)
- `/lumo_lab/state/state.json`
- `/lumo_lab/state/decisions.json`

## Local Lumo API V2 in Container

The image installs `carlostkd/Lumo-Api-V2` and runs it under `supervisord`.

### Auth bootstrap for Lumo API V2

Lumo API V2 requires an auth session file (`auth.json`) produced by its login flow.

For this project, place that file at:

- `/lumo_lab/config/lumo-auth.json`

`start-lumo.sh` copies it into the Lumo runtime directory before launching `node lumo.js`.

If missing, the Lumo process logs a warning and idles instead of crashing the whole container.

### Disable local Lumo process

Set:

```env
LUMO_LOCAL_ENABLED=false
```

Then point `LUMO_BASE_URL` to an external Lumo service.

## Web Configuration

`/api/config` supports:

- `lumo.baseUrl`
- `lumo.apiKey`
- `lumo.classifyPath`
- `labels.allowlist`
- timezone/log/scan/rate-limit settings

The Config page currently focuses on authentication and connectivity checks:

- Separate Lumo auth upload from `scripts/generate_lumo_auth.js` to:
  - `POST /api/lumo/auth`
- Separate Mail auth upload from `scripts/generate_mail_auth.js` to:
  - `POST /api/proton/auth`
- Lumo connectivity test (`POST /api/lumo/test`)

Tuning is managed in the dedicated Tuning tab and through `/api/tuning`.

## Tuning and Guardrails

The backend prepends `GARDRAIL.md` and `TUNING.md` before each classify request.

Default tuning file resolution order:

1. `TUNING_FILE` environment path (if set)
2. `/lumo_lab/config/TUNING.md`
3. `TUNING.md` (repo root)
4. `/opt/lumo-lab/TUNING.md`

You can read and update runtime tuning through:

- `GET /api/tuning`
- `PUT /api/tuning`

Local Lumo auth file management is available through:

- `GET /api/lumo/auth`
- `POST /api/lumo/auth`

`POST /api/lumo/auth` accepts the `auth.json` generated locally by the upstream Lumo API V2 auth flow, stores it in `/lumo_lab/config/lumo-auth.json`, and attempts to restart only the `lumo` supervised process.

Proton auth conversion/status is available through:

- `GET /api/proton/auth`
- `POST /api/proton/auth`

`POST /api/proton/auth` accepts Playwright storageState `auth.json`, extracts token pairs from Proton `AUTH-*` and `REFRESH-*` cookies, and writes `/lumo_lab/config/proton-auth.json` for backend runtime use.

## Lumo Test Button

The Config page includes **Run Lumo Test**.

It calls backend endpoint:

- `POST /api/lumo/test`

Payload:

```json
{
  "prompt": "Email Address: test@example.com\nSubject Line: Lumo connectivity test\nReturn only the label Questionable"
}
```

Server-side timeout for this endpoint is 120 seconds.
Response includes connection target and returned text.

## Authentication and Security

- Local username/password login (`/api/auth/login`)
- First-run default credentials are bootstrapped as `admin` / `ChangeMeNow123!`
- First login is forced to change password (`MUST_CHANGE_PASSWORD=true`)
- Session cookie (`lumo_session`) with sliding expiry
- Auth middleware protects operational routes
- `/api/health`, `/api/auth/login`, `/api/auth/me`, `/api/setup` are public by design
- API key and sensitive values can be configured in YAML and/or environment overrides

## API Endpoints (Current)

Public:

- `GET /api/health`
- `POST /api/auth/login`
- `GET /api/auth/me`
- `GET /api/setup`

Protected (session required):

- `POST /api/auth/logout`
- `POST /api/auth/password`
- `GET /api/status`
- `GET|PUT /api/config`
- `GET /api/labels`
- `GET|PUT /api/tuning`
- `GET /api/decisions`
- `GET /api/logs`
- `GET|POST /api/lumo/auth`
- `GET|POST /api/proton/auth`
- `POST /api/lumo/test`
- `POST /api/health/repair`

`GET /api/labels` returns both configured labels and Proton-discovered labels:

```json
{
  "configured": ["Questionable", "Primary"],
  "proton": ["Important", "Primary", "Promotions", "Social", "Updates"]
}
```

## Proton Configuration

Supported runtime mode:

1. File mode:
- `PROTON_AUTH_FILE` (default `/lumo_lab/config/proton-auth.json`)
- `PROTON_APP_VERSION` (optional, forwarded as `x-pm-appversion`)
- `PROTON_APP_VERSION_FALLBACKS` (optional comma-separated fallback versions used when Proton returns 422 out-of-date)

Use the Config page Mail auth upload (`scripts/generate_mail_auth.js`) to convert and persist Proton tokens.

## Development Commands

Backend:

```bash
cd backend
go mod tidy
go build ./...
```

Frontend:

```bash
cd frontend
npm install
npm run build
npm run dev
```

## Logging

- Console logs enabled
- Rotating file logs at `/lumo_lab/logs/app.log`
- Rotation policy: 16 MB, up to 8 rotated files
- Local Lumo process logs at `/lumo_lab/logs/lumo.log`
- Lumo adapter failure logs at `/lumo_lab/logs/lumo.err.log`
- Successful/parsed Lumo output lines are prefixed with `[LUMO OUTPUT]`
- Lumo classify/request failures are prefixed with `[LUMO ERROR]`

## Known Limitations

- Local session-based auth is in-memory and not yet distributed/multi-instance aware
- Lumo API V2 itself requires manual session bootstrap (`auth.json`) from upstream flow
- Frontend still has placeholder pages for some advanced operational views
- Tuning editor parsing is best-effort for common markdown patterns; unusual custom formats may need manual adjustment

## Troubleshooting

### Lumo test fails

- Verify `lumo.baseUrl` and `lumo.classifyPath`
- Check `/api/logs?lines=200`
- Confirm `/lumo_lab/config/lumo-auth.json` exists for local Lumo mode
- Confirm local Lumo process logs in `/lumo_lab/logs/lumo.log`
- Check `/lumo_lab/logs/lumo.err.log` for `[LUMO ERROR]` entries
- If UI error is 401, re-login (session expired)
- If UI error is 502, inspect upstream Lumo error text in `lumo.err.log`

### Proton fetch returns 422 out of date

- Re-upload fresh Proton auth via Config shared auth upload
- Restart daemon or wait for next poll tick
- If needed, set `PROTON_APP_VERSION` in `.env` and redeploy

### Unauthorized API responses

- Login first via web UI
- Check `/api/auth/me`

### No labels applied

- Ensure `labels.allowlist` is not empty
- Verify Proton credentials and unread inbox state
- Check daemon logs and decisions endpoint

## License

This repository is licensed under AGPL V3.0.

Respect upstream licenses for dependencies. 

The Lumo API V2 is licensed under AGPL v3.0

The Proton API SDK is licensed by Proton AG under The MIT License.
