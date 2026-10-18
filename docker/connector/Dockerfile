# Obraz konektora tunelu (zamiast cloudflared): kontener obok aplikacji.
#   make connector-image   albo
#   docker build -t entrypoint-connector:0.1.0 -f docker/connector/Dockerfile dist/connector/linux_amd64
FROM debian:trixie-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/*
COPY entrypoint-connector frpc /usr/local/bin/
USER 65534:65534
ENTRYPOINT ["entrypoint-connector", "tunnel"]
