# Entrypoint: build, release packages and e2e tests.
#
#   make panel           web panel frontend (panel/ -> gateway/internal/panel/ui)
#   make dist            gateway release tarball (dist/release/) and connector
#   make sdk             npm SDK packages (dist/npm/)
#   make release         GitHub release files (dist/release-assets/)
#   make test            unit tests
#   make e2e             e2e tests of our code in Docker (SHARDS=4 parallel shards)
#   make e2e-experiments experiments: Caddy and frp behavior (after changing versions.json)
#   make e2e SUITE=Routing   one suite (names: Smoke Install TLS Routing Tunnels
#                        Replicas State Revoke Ops SDK Resilience Secrets VPN Panel Limits Update)
#   make e2e-up / e2e-down   environment without running tests

SHELL := /bin/bash
REPO := $(abspath .)
VERSION := $(shell python3 -c 'import json;print(json.load(open("versions.json"))["entrypoint"])')
GOFLAGS_BUILD := -trimpath -ldflags "-s -w -X github.com/k18k/entrypoint/gateway/internal/version.Version=$(VERSION)"
COMPOSE := REPO=$(REPO) docker compose -f e2e/docker-compose.yml
PLATFORMS ?= linux_amd64
SUITE ?=
RUN ?=
SHARDS ?= 4
KEEP ?=

.PHONY: all build deps panel dist sdk release connector-image test e2e e2e-experiments e2e-up e2e-down e2e-build e2e-shell clean

all: dist sdk

deps:
	python3 scripts/fetch-deps.py $(PLATFORMS)

# Web panel frontend, embedded into entrypoint-panel.
panel:
	cd panel && npm ci --no-audit --no-fund && npm run build
	find gateway/internal/panel/ui -mindepth 1 ! -name .gitkeep -delete
	cp -r panel/dist/. gateway/internal/panel/ui/

build: panel
	@for p in $(PLATFORMS); do \
	  os=$${p%_*}; arch=$${p#*_}; ext=""; [ $$os = windows ] && ext=.exe; \
	  out=dist/bin/$$p; mkdir -p $$out; \
	  if [ $$os = linux ]; then \
	    (cd gateway && CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build $(GOFLAGS_BUILD) -o ../$$out/ ./cmd/entrypoint ./cmd/entrypointd ./cmd/entrypoint-panel) || exit 1; \
	  fi; \
	  (cd gateway && CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build $(GOFLAGS_BUILD) -o ../$$out/entrypoint-connector$$ext ./cmd/entrypoint-connector) || exit 1; \
	done

dist: deps build
	@for p in $(filter linux_%,$(PLATFORMS)); do \
	  name=entrypoint_$(VERSION)_$$p; d=dist/release/$$name; rm -rf $$d; mkdir -p $$d/bin; \
	  cp dist/bin/$$p/entrypoint dist/bin/$$p/entrypointd dist/bin/$$p/entrypoint-panel $$d/bin/; \
	  cp dist/deps/$$p/caddy dist/deps/$$p/frps dist/deps/$$p/headscale dist/deps/$$p/tailscale dist/deps/$$p/tailscaled $$d/bin/; \
	  cp versions.json $$d/; echo $(VERSION) > $$d/VERSION; \
	  tar -C dist/release -czf dist/release/$$name.tar.gz $$name; \
	  (cd dist/release && sha256sum $$name.tar.gz > $$name.tar.gz.sha256); \
	  echo "dist/release/$$name.tar.gz"; \
	done
	@for p in $(PLATFORMS); do \
	  mkdir -p dist/connector/$$p; ext=""; [ $${p%_*} = windows ] && ext=.exe; \
	  cp dist/bin/$$p/entrypoint-connector$$ext dist/deps/$$p/frpc$$ext dist/connector/$$p/; \
	done
	cp scripts/install.sh dist/release/install.sh

# npm packages: the SDK and the per-platform packages with the connector and frpc.
sdk: dist
	cd sdk/node && npm install --no-audit --no-fund && npm run check && npm run build
	mkdir -p dist/npm
	cd sdk/node && npm pack --pack-destination ../../dist/npm >/dev/null
	@for p in $(PLATFORMS); do \
	  np=$$(case $$p in linux_amd64) echo linux-x64;; linux_arm64) echo linux-arm64;; darwin_amd64) echo darwin-x64;; darwin_arm64) echo darwin-arm64;; windows_amd64) echo win32-x64;; esac); \
	  d=dist/npm-src/$$np; rm -rf $$d; mkdir -p $$d/bin; \
	  cp dist/connector/$$p/* $$d/bin/; \
	  node scripts/platform-package.mjs $$np $(VERSION) $$d; \
	  (cd $$d && npm pack --pack-destination ../../npm >/dev/null); \
	done
	@ls dist/npm

# All release files (linux_amd64 gateway, connector and frpc for all platforms, SDK).
RELEASE_PLATFORMS := linux_amd64 linux_arm64 darwin_amd64 darwin_arm64 windows_amd64
release:
	$(MAKE) dist sdk PLATFORMS="$(RELEASE_PLATFORMS)"
	python3 scripts/release-assets.py assets

# Connector image for applications running in Docker.
connector-image: dist
	docker build -t entrypoint-connector:$(VERSION) -f docker/connector/Dockerfile dist/connector

test:
	cd gateway && go vet ./... && go test ./...
	cd e2e && go vet ./...

e2e/.run:
	mkdir -p e2e/.run e2e/artifacts

e2e-build: dist sdk e2e/.run
	mkdir -p dist/e2e
	cd e2e && CGO_ENABLED=0 go build -o ../dist/e2e/testsrv ./cmd/testsrv
	scripts/e2e-update-releases.sh dist/deps/linux_amd64
	docker build -q -t ep-e2e-gateway e2e/images/gateway >/dev/null
	docker build -q -t ep-e2e-client --build-arg VERSION=$(VERSION) -f e2e/images/client/Dockerfile . >/dev/null
	docker build -q -t ep-e2e-client-node22 --build-arg VERSION=$(VERSION) --build-arg NODE_IMAGE=node:22-trixie-slim -f e2e/images/client/Dockerfile . >/dev/null
	docker build -q -t ep-e2e-upstream -f e2e/images/upstream/Dockerfile . >/dev/null
	docker build -q -t ep-e2e-chaos e2e/images/chaos >/dev/null
	docker build -q -t ep-e2e-connector -f docker/connector/Dockerfile dist/connector >/dev/null
	docker build -q -t ep-e2e-runner e2e/images/runner >/dev/null

# Tests of our code (before a commit), in SHARDS parallel environment copies.
e2e: e2e-build
	e2e/run.sh e2e $(SHARDS) '$(if $(RUN),$(RUN),^Test$(SUITE))' $(KEEP)

# Experiments: Caddy and frp behavior the specification relies on
# (run after changing versions in versions.json).
e2e-experiments: e2e-build
	e2e/run.sh experiments $(SHARDS) '$(if $(RUN),$(RUN),^Test$(SUITE))' $(KEEP)

e2e-up: e2e-build
	mkdir -p e2e/.run/0/zones e2e/.run/0/shared
	cp -f e2e/dns/ep.test.db e2e/.run/0/zones/ep.test.db
	docker volume create ep-e2e-gocache >/dev/null
	$(COMPOSE) up -d --force-recreate --remove-orphans

e2e-down:
	@for s in $$(seq 0 7); do SHARD=$$s NET=10.77.$$s PRIV=10.78.$$s $(COMPOSE) --profile restore --profile tools down --remove-orphans >/dev/null 2>&1; done

e2e-shell:
	$(COMPOSE) exec runner bash

clean:
	rm -rf dist e2e/.run e2e/artifacts sdk/node/lib
