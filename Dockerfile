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
	&& apt-get install -y --no-install-recommends supervisor tzdata curl ca-certificates lsof perl zstd \
	&& rm -rf /var/lib/apt/lists/* \
	&& useradd -m -s /bin/bash lumolab

WORKDIR /opt/lumo-lab
COPY --from=backend-builder /app/bin/lumo-lab /usr/local/bin/lumo-lab
COPY --from=frontend-builder /frontend/dist /opt/lumo-lab/frontend
COPY TUNING.md /opt/lumo-lab/TUNING.md
COPY supervisord.conf /etc/supervisord.conf
COPY scripts /opt/lumo-lab/scripts

RUN chmod +x /opt/lumo-lab/scripts/*.sh

RUN curl -fsSL https://ollama.com/install.sh | sh

ENV CONFIG_DIR=/lumo_lab/config
ENV LOG_DIR=/lumo_lab/logs
ENV STATE_DIR=/lumo_lab/state
ENV WEB_PORT=5866
ENV TZ=America/New_York
ENV OLLAMA_BASE_URL=http://127.0.0.1:11434
ENV OLLAMA_MODEL=qwen3:1.7b
ENV OLLAMA_MODELS=/lumo_lab/state/ollama/models

RUN mkdir -p /lumo_lab/config /lumo_lab/logs /lumo_lab/state \
	&& mkdir -p /lumo_lab/state/ollama/models \
	&& chown -R lumolab:lumolab /lumo_lab /opt/lumo-lab

VOLUME ["/lumo_lab/config", "/lumo_lab/logs", "/lumo_lab/state"]
EXPOSE 5866

CMD ["/opt/lumo-lab/scripts/entrypoint.sh"]
