
<img src="./lumolabel.png" alt="Lumo Labels" />

# Lumo Labels

Lumo Labels is a Dockerized Proton Mail auto-labeler. It polls unread inbox mail, classifies each message with the local Lumo model, and applies the matching Proton label.

The runtime is a single Docker container managed by `supervisord`. It runs the Go API server, the polling daemon, and the local Lumo API V2 process together.

## What It Does

For each unread inbox message, the app:

1. Fetches the message from Proton Mail.
2. Redacts configured sensitive content.
3. Builds a classification prompt using `GARDRAIL.md`, `TUNING.md`, sender, subject, body, and recent applied decisions.
4. Sends the prompt to Lumo.
5. Parses the response against the allowed label list.
6. Applies the label in Proton Mail when a match is found.
7. Persists checkpoints and decisions so messages are not reprocessed unnecessarily.

## Current UI

The web UI currently includes these sections:

- Login / Change Password
- Config
- Tuning
- Decisions
- Logs
- Labels

## Requirements

- Docker
- Docker Compose

Optional for local development outside Docker:

- Go 1.22+
- Node.js 20+
- npm

## Setup

1. Copy Enviroment:

```bash
git copy https://github.com/Yoshiofthewire/Lumo-Labels.git
```

2. Generate Auth Tokens:

```bash
node generate_lumo_auth.js
node generate_mail_auth.js
```

3. Start the stack:

```bash
docker compose up --build -d
```

4. Open the app:

- Web UI: http://localhost:5866

5. Log in with the bootstrap account:

- Username: `admin`
- Password: `ChangeMeNow123!`

You will be prompted to change the password after the first login.

6. On the Config tab upload the generated auth.json files.

## Running

To start the app after the image is built:

```bash
docker compose up -d
```

To rebuild after code changes:

```bash
docker compose up --build -d
```

To restart just the backend runtime after state or config changes:

```bash
docker compose exec -T lumo-lab supervisorctl restart daemon
```

## Development Checks

These are the main build checks for the repo:

```bash
cd backend && go build ./...
cd frontend && npm run build
```

## Runtime Data

The container persists its data in three named volumes mapped to:

- `/lumo_lab/config`
- `/lumo_lab/logs`
- `/lumo_lab/state`

Important runtime files:

- `/lumo_lab/config/config.yaml`
- `/lumo_lab/config/admin.env`
- `/lumo_lab/config/proton-auth.json`
- `/lumo_lab/config/lumo-auth.json`
- `/lumo_lab/config/TUNING.md`
- `/lumo_lab/state/state.json`
- `/lumo_lab/state/decisions.json`

## Auth and Setup Notes

- The app uses session-based login with the `lumo_session` cookie.
- First login is forced into the change-password flow.
- The login page is also the change-password entry point once authenticated.
- Runtime tuning is loaded from `TUNING.md` and the allowed label list is parsed from the `## Allowed Labels` section.
- If `labels.allowlist` is empty in config, the backend auto-populates it from `TUNING.md`.

## Logs and Decisions

- Polling activity is written to `daemon.log`.
- Lumo warmup and classify activity are written to `lumo-server.log`.
- Decision history is stored in `/lumo_lab/state/decisions.json` and shown in the Decisions tab.
- The Decisions page auto-refreshes so recent applied labels appear without manual reload.

## Local Lumo API V2

The Docker image installs and starts the upstream Lumo API V2 project inside the container.

If you want to disable the bundled Lumo process and point at another service, set:

```env
LUMO_LOCAL_ENABLED=false
```

Then update `LUMO_BASE_URL` to the external endpoint.

## Project Structure

- `backend/` - Go backend, API server, poller, adapters, and state store
- `frontend/` - React + Vite web UI
- `scripts/` - bootstrap and runtime helper scripts
- `Dockerfile` - single-image build for backend, frontend, and Lumo runtime
- `docker-compose.yml` - local orchestration and volume wiring
- `supervisord.conf` - process supervision inside the container

## Notes

- `GARDRAIL.md` is prepended to every classify request.
- `TUNING.md` is prepended to every classify request and is editable from the UI.
- The poller performs an unread sweep on startup after warmup so new labels can be applied immediately.
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
