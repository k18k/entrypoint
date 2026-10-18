FROM docker:27.5.1-cli AS docker
FROM golang:1.27-trixie
COPY --from=docker /usr/local/bin/docker /usr/local/bin/docker
COPY --from=docker /usr/local/libexec/docker/cli-plugins/docker-compose /usr/local/libexec/docker/cli-plugins/docker-compose
ENV GOTOOLCHAIN=local
