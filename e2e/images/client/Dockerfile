# Context: the repository root.
ARG NODE_IMAGE=node:24-trixie-slim
FROM ${NODE_IMAGE}
ARG VERSION
ARG PLATFORM=linux_amd64
ARG NPM_PLATFORM=linux-x64
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl iproute2 procps \
 && rm -rf /var/lib/apt/lists/*
COPY dist/connector/${PLATFORM}/frpc /usr/local/bin/frpc
# A VPN node (suite VPN), in userspace mode: no TUN device needed.
COPY dist/deps/${PLATFORM}/tailscale dist/deps/${PLATFORM}/tailscaled /usr/local/bin/
WORKDIR /app
COPY e2e/apps/package.json ./
COPY dist/npm/ /tmp/npm/
# The platform package comes from the local build, not the registry (hence --omit=optional).
RUN npm install --no-audit --no-fund --omit=optional \
      /tmp/npm/k18k-entrypoint-sdk-${VERSION}.tgz /tmp/npm/k18k-entrypoint-sdk-${NPM_PLATFORM}-${VERSION}.tgz ws@8.22.0 \
 && rm -rf /tmp/npm \
 && ln -s /app/node_modules/@k18k/entrypoint-sdk-${NPM_PLATFORM}/bin/entrypoint-connector /usr/local/bin/entrypoint-connector
COPY e2e/apps/ ./
CMD ["sleep", "infinity"]
