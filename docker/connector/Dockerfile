# Obraz konektora tunelu (zamiast cloudflared): kontener obok aplikacji.
# Kontekst: dist/connector/ (podkatalogi <os>_<arch> z entrypoint-connector i frpc).
#   make connector-image
#   docker run -e ENTRYPOINT_CONNECT=... ghcr.io/k18k/entrypoint-connector:0.1.1 --upstream http://app:3000
FROM debian:trixie-slim
ARG TARGETOS=linux
ARG TARGETARCH=amd64
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/*
COPY ${TARGETOS}_${TARGETARCH}/entrypoint-connector ${TARGETOS}_${TARGETARCH}/frpc /usr/local/bin/
USER 65534:65534
ENTRYPOINT ["entrypoint-connector", "tunnel"]
