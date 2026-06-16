FROM golang:1.26.4 AS backend-builder
WORKDIR /app
COPY backend/go.mod backend/go.sum* ./backend/
RUN cd backend && go mod download
COPY backend ./backend
RUN cd backend && go build -o /app/bin/lumo-lab ./cmd/main.go

FROM node:20-alpine AS frontend-builder
WORKDIR /frontend
COPY frontend/package.json frontend/package-lock.json* ./
RUN npm install
COPY frontend .
RUN npm run build

FROM node:26.3.0-slim
RUN apt-get update \
	&& apt-get remove -y perl perl-base --allow-remove-essential \
	&& apt-get install -y --no-install-recommends supervisor tzdata git ca-certificates lsof \
	&& rm -rf /var/lib/apt/lists/* \
	&& useradd -m -s /bin/bash lumolab

ENV PLAYWRIGHT_BROWSERS_PATH=/opt/ms-playwright

WORKDIR /opt/lumo-lab
COPY --from=backend-builder /app/bin/lumo-lab /usr/local/bin/lumo-lab
COPY --from=frontend-builder /frontend/dist /opt/lumo-lab/frontend
COPY GARDRAIL.md /opt/lumo-lab/GARDRAIL.md
COPY TUNING.md /opt/lumo-lab/TUNING.md
COPY supervisord.conf /etc/supervisord.conf
COPY scripts /opt/lumo-lab/scripts

RUN chmod +x /opt/lumo-lab/scripts/*.sh

RUN git clone --depth 1 https://github.com/carlostkd/Lumo-Api-V2.git /opt/lumo-api-v2 \
	&& cd /opt/lumo-api-v2 \
	&& npm install playwright \
	&& npx playwright install --with-deps firefox

ENV CONFIG_DIR=/lumo_lab/config
ENV LOG_DIR=/lumo_lab/logs
ENV STATE_DIR=/lumo_lab/state
ENV WEB_PORT=5866
ENV TZ=America/New_York
ENV LUMO_LOCAL_ENABLED=true
ENV LUMO_LOCAL_DIR=/opt/lumo-api-v2
ENV LUMO_AUTH_FILE=/lumo_lab/config/lumo-auth.json

RUN mkdir -p /lumo_lab/config /lumo_lab/logs /lumo_lab/state \
	&& mkdir -p /opt/ms-playwright \
	&& chown -R lumolab:lumolab /lumo_lab /opt/lumo-lab /opt/lumo-api-v2

VOLUME ["/lumo_lab/config", "/lumo_lab/logs", "/lumo_lab/state"]
EXPOSE 5866

CMD ["/opt/lumo-lab/scripts/entrypoint.sh"]
