FROM golang:1.26.4 AS backend-builder
WORKDIR /app
COPY backend/go.mod backend/go.sum* ./backend/
RUN cd backend && go mod download
COPY backend ./backend
RUN cd backend && go build -o /app/bin/llama-lab ./cmd/main.go

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
	&& useradd -m -s /bin/bash llamalab

WORKDIR /opt/llama-lab
COPY --from=backend-builder /app/bin/llama-lab /usr/local/bin/llama-lab
COPY --from=frontend-builder /frontend/dist /opt/llama-lab/frontend
COPY TUNING.md /opt/llama-lab/TUNING.md
COPY supervisord.conf /etc/supervisord.conf
COPY scripts /opt/llama-lab/scripts

RUN chmod +x /opt/llama-lab/scripts/*.sh

RUN curl -fsSL https://ollama.com/install.sh | sh

ENV CONFIG_DIR=/llama_lab/config
ENV SECRET_DIR=/llama_lab/private
ENV LOG_DIR=/llama_lab/logs
ENV STATE_DIR=/llama_lab/state
ENV WEB_PORT=5866
ENV TZ=America/New_York
ENV OLLAMA_BASE_URL=http://127.0.0.1:11434
ENV OLLAMA_MODEL=nemotron-3-nano:4b
ENV OLLAMA_MODELS=/llama_lab/ollama-models
ENV PROTON_PRIVATE_KEY_FILE=/llama_lab/private/proton-private-key.asc
ENV PROTON_PRIVATE_KEY_PASSWORD_FILE=/llama_lab/private/proton-private-key-password

RUN mkdir -p /llama_lab/config /llama_lab/private /llama_lab/logs /llama_lab/state \
	&& mkdir -p /llama_lab/ollama-models \
	&& chown -R llamalab:llamalab /llama_lab /opt/llama-lab

VOLUME ["/llama_lab/config", "/llama_lab/private", "/llama_lab/logs", "/llama_lab/state"]
EXPOSE 5866

CMD ["/opt/llama-lab/scripts/entrypoint.sh"]
