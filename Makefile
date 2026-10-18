# Entrypoint — budowanie, paczki wydania i testy e2e.
#
#   make dist            paczka wydania bramy (dist/release/) i konektor
#   make sdk             paczki npm SDK (dist/npm/)
#   make test            testy jednostkowe
#   make e2e             pełny zestaw e2e w Dockerze
#   make e2e SUITE=Routing   jeden zestaw (nazwy: Smoke Install TLS Routing Tunnels
#                        Replicas State Revoke Ops SDK Resilience Secrets)
#   make e2e-up / e2e-down   środowisko bez uruchamiania testów

SHELL := /bin/bash
REPO := $(abspath .)
VERSION := $(shell python3 -c 'import json;print(json.load(open("versions.json"))["entrypoint"])')
GOFLAGS_BUILD := -trimpath -ldflags "-s -w -X github.com/k18k/entrypoint/gateway/internal/version.Version=$(VERSION)"
COMPOSE := REPO=$(REPO) docker compose -f e2e/docker-compose.yml
PLATFORMS ?= linux_amd64
SUITE ?=
RUN ?=

.PHONY: all build deps dist sdk connector-image test e2e e2e-up e2e-down e2e-build e2e-shell clean

all: dist sdk

deps:
	python3 scripts/fetch-deps.py $(PLATFORMS)

build:
	@for p in $(PLATFORMS); do \
	  os=$${p%_*}; arch=$${p#*_}; ext=""; [ $$os = windows ] && ext=.exe; \
	  out=dist/bin/$$p; mkdir -p $$out; \
	  if [ $$os = linux ]; then \
	    (cd gateway && CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build $(GOFLAGS_BUILD) -o ../$$out/ ./cmd/entrypoint ./cmd/entrypointd) || exit 1; \
	  fi; \
	  (cd gateway && CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build $(GOFLAGS_BUILD) -o ../$$out/entrypoint-connector$$ext ./cmd/entrypoint-connector) || exit 1; \
	done

dist: deps build
	@for p in $(filter linux_%,$(PLATFORMS)); do \
	  name=entrypoint_$(VERSION)_$$p; d=dist/release/$$name; rm -rf $$d; mkdir -p $$d/bin; \
	  cp dist/bin/$$p/entrypoint dist/bin/$$p/entrypointd $$d/bin/; \
	  cp dist/deps/$$p/caddy dist/deps/$$p/frps $$d/bin/; \
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

# Pakiety npm: SDK i pakiety platform z konektorem i frpc.
sdk: dist
	cd sdk/node && npm install --no-audit --no-fund && npm run check && npm run build
	mkdir -p dist/npm
	cd sdk/node && npm pack --pack-destination ../../dist/npm >/dev/null
	@for p in $(PLATFORMS); do \
	  np=$$(case $$p in linux_amd64) echo linux-x64;; linux_arm64) echo linux-arm64;; darwin_amd64) echo darwin-x64;; darwin_arm64) echo darwin-arm64;; windows_amd64) echo win32-x64;; esac); \
	  os=$${np%-*}; cpu=$${np#*-}; d=dist/npm-src/$$np; rm -rf $$d; mkdir -p $$d/bin; \
	  cp dist/connector/$$p/* $$d/bin/; \
	  printf '{\n  "name": "@k18k/entrypoint-%s",\n  "version": "%s",\n  "description": "Konektor Entrypoint i frpc dla %s",\n  "license": "MIT",\n  "os": ["%s"],\n  "cpu": ["%s"],\n  "files": ["bin"]\n}\n' $$np $(VERSION) $$np $$os $$cpu > $$d/package.json; \
	  (cd $$d && npm pack --pack-destination ../../npm >/dev/null); \
	done
	@ls dist/npm

# Obraz konektora dla aplikacji w Dockerze.
connector-image: dist
	docker build -t entrypoint-connector:$(VERSION) -f docker/connector/Dockerfile dist/connector/linux_amd64

test:
	cd gateway && go vet ./... && go test ./...
	cd e2e && go vet ./...

e2e/.run:
	mkdir -p e2e/.run/zones e2e/.run/shared e2e/artifacts

e2e-build: dist sdk e2e/.run
	mkdir -p dist/e2e
	cd e2e && CGO_ENABLED=0 go build -o ../dist/e2e/testsrv ./cmd/testsrv
	rm -f e2e/.run/zones/ep.test.db && cp e2e/dns/ep.test.db e2e/.run/zones/ep.test.db
	docker build -q -t ep-e2e-gateway e2e/images/gateway
	docker build -q -t ep-e2e-client -f e2e/images/client/Dockerfile .
	docker build -q -t ep-e2e-client-node22 --build-arg NODE_IMAGE=node:22-trixie-slim -f e2e/images/client/Dockerfile .
	docker build -q -t ep-e2e-upstream -f e2e/images/upstream/Dockerfile .
	docker build -q -t ep-e2e-chaos e2e/images/chaos
	docker build -q -t ep-e2e-connector -f docker/connector/Dockerfile dist/connector/linux_amd64
	docker build -q -t ep-e2e-runner e2e/images/runner

e2e-up: e2e-build
	$(COMPOSE) up -d --force-recreate --remove-orphans

e2e-down:
	$(COMPOSE) --profile restore --profile tools down -v --remove-orphans

# Testy uruchamia kontener runner; artefakty trafiają do e2e/artifacts/.
e2e: e2e-up
	$(COMPOSE) exec -T runner go test -p 1 -count=1 -timeout 120m -v \
	  -run '$(if $(RUN),$(RUN),^Test$(SUITE))' ./tests/ 2>&1 | tee e2e/artifacts/e2e.log; \
	  status=$${PIPESTATUS[0]}; \
	  $(COMPOSE) exec -T runner chown -R $$(id -u):$$(id -g) $(REPO)/e2e/artifacts $(REPO)/e2e/.run; \
	  $(if $(KEEP),,$(MAKE) --no-print-directory e2e-down;) exit $$status

e2e-shell:
	$(COMPOSE) exec runner bash

clean:
	rm -rf dist e2e/.run e2e/artifacts sdk/node/lib
