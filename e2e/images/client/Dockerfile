# Kontekst: katalog główny repozytorium.
ARG NODE_IMAGE=node:24-trixie-slim
FROM ${NODE_IMAGE}
ARG VERSION
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl iproute2 procps \
 && rm -rf /var/lib/apt/lists/*
COPY dist/connector/linux_amd64/frpc /usr/local/bin/frpc
WORKDIR /app
COPY e2e/apps/package.json ./
COPY dist/npm/ /tmp/npm/
RUN ENTRYPOINT_SKIP_CONNECTOR_DOWNLOAD=1 npm install --no-audit --no-fund --omit=optional \
      /tmp/npm/k18k-entrypoint-${VERSION}.tgz /tmp/npm/k18k-entrypoint-linux-x64-${VERSION}.tgz ws@8.22.0 \
 && rm -rf /tmp/npm
COPY e2e/apps/ ./
CMD ["sleep", "infinity"]
