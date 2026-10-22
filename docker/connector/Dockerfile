# Tunnel connector image (instead of cloudflared): a container next to the application.
# Context: dist/connector/ (<os>_<arch> subdirectories with entrypoint-connector and frpc).
#   make connector-image
#   docker run -e ENTRYPOINT_CONNECT=... ghcr.io/k18k/entrypoint-connector:0.1.1 --upstream http://app:3000
#   docker run -e ENTRYPOINT_VPN=... -v vpn:/state ghcr.io/k18k/entrypoint-connector:0.1.1 vpn --name db --expose 5432=db:5432
FROM debian:trixie-slim
ARG TARGETOS=linux
ARG TARGETARCH=amd64
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/* \
 && mkdir /state && chown 65534:65534 /state
# VPN node identity (vpn mode): mount a volume here so restarts keep the node.
ENV XDG_CONFIG_HOME=/state
COPY ${TARGETOS}_${TARGETARCH}/entrypoint-connector ${TARGETOS}_${TARGETARCH}/frpc /usr/local/bin/
USER 65534:65534
ENTRYPOINT ["entrypoint-connector"]
CMD ["tunnel"]
