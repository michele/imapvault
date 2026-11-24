GOLANGCI_LINT_VERSION ?= v2.15.0
GORELEASER_VERSION ?= v2.20.3
CONTAINER ?= $(shell command -v docker >/dev/null 2>&1 && echo docker || echo podman)
IMAGE ?= ghcr.io/michele/imapvault:latest
PLATFORMS ?= linux/amd64,linux/arm64
COMPOSE ?= $(shell command -v docker >/dev/null 2>&1 && echo "docker compose" || echo "podman compose")
# Lazily expanded (?= and =), so only build shells out to git.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
REVISION ?= $(shell git rev-parse --verify -q HEAD 2>/dev/null || echo unknown)
LDFLAGS = -X main.version=$(VERSION) -X main.commit=$(REVISION)
DOCKER_BUILD_ARGS = --label org.opencontainers.image.version=$(VERSION) --label org.opencontainers.image.revision=$(REVISION) \
	--build-arg VERSION=$(VERSION) --build-arg COMMIT=$(REVISION)
COMPOSE_FILE := docker-compose.test.yml

.PHONY: test lint itest itest-up itest-down seed deploy-itest build release-snapshot docker-build docker-buildx

test:
	go test ./... -race

lint:
	go vet ./...
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/imapvault ./cmd/imapvault

# All release archives into ./dist, built locally and published nowhere.
release-snapshot:
	go run github.com/goreleaser/goreleaser/v2@$(GORELEASER_VERSION) release --snapshot --clean

# Container image for this machine's architecture, tagged imapvault:local.
docker-build:
	$(CONTAINER) build $(DOCKER_BUILD_ARGS) -t imapvault:local .

# Multi-arch image (amd64 + arm64) named $(IMAGE). The build stage cross-compiles,
# so no QEMU is needed. Only PUSH=1 pushes.
docker-buildx:
ifeq ($(CONTAINER),podman)
	-podman manifest rm $(IMAGE) 2>/dev/null
	podman build $(DOCKER_BUILD_ARGS) --platform $(PLATFORMS) --manifest $(IMAGE) .
	$(if $(filter 1,$(PUSH)),podman manifest push --all $(IMAGE) docker://$(IMAGE))
else
	docker buildx build $(DOCKER_BUILD_ARGS) --platform $(PLATFORMS) -t $(IMAGE) $(if $(filter 1,$(PUSH)),--push) .
endif

itest-up:
	$(COMPOSE) -f $(COMPOSE_FILE) up -d --wait

itest-down:
	$(COMPOSE) -f $(COMPOSE_FILE) down -v

seed:
	go run ./tools/seed --addr 127.0.0.1:1143 --user me@imapvault.test --dir testdata/seed

itest: itest-up seed
	IMAPVAULT_IMAP_PASSWORD=test go test -tags integration -race -count=1 -p 1 ./... ; \
	status=$$?; $(MAKE) itest-down; exit $$status

# The compose bundle in deploy/, run as a user would run it, against the dovecot server.
deploy-itest: docker-build itest-up seed
	COMPOSE="$(COMPOSE)" tools/deploy-itest.sh ; \
	status=$$?; $(MAKE) itest-down; exit $$status
