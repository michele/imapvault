GOLANGCI_LINT_VERSION ?= v2.15.0
COMPOSE ?= $(shell command -v docker >/dev/null 2>&1 && echo "docker compose" || echo "podman compose")
# Lazily expanded (?= and =), so only build shells out to git.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
REVISION ?= $(shell git rev-parse --verify -q HEAD 2>/dev/null || echo unknown)
LDFLAGS = -X main.version=$(VERSION) -X main.commit=$(REVISION)
COMPOSE_FILE := docker-compose.test.yml

.PHONY: test lint itest itest-up itest-down seed build

test:
	go test ./... -race

lint:
	go vet ./...
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/imapvault ./cmd/imapvault

itest-up:
	$(COMPOSE) -f $(COMPOSE_FILE) up -d --wait

itest-down:
	$(COMPOSE) -f $(COMPOSE_FILE) down -v

seed:
	go run ./tools/seed --addr 127.0.0.1:1143 --user me@imapvault.test --dir testdata/seed

itest: itest-up seed
	IMAPVAULT_IMAP_PASSWORD=test go test -tags integration -race -count=1 ./... ; \
	status=$$?; $(MAKE) itest-down; exit $$status
