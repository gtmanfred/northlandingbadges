.PHONY: help build test test-short race cover lint fmt vet staticcheck golden run docker clean

BINARY  := bin/server
VERSION ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)

help: ## Show available targets
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

build: ## Build the server binary
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o $(BINARY) ./cmd/server

test: ## Run the full suite with the race detector
	go test -race -shuffle=on ./...

test-short: ## Run tests without the race detector
	go test ./...

cover: ## Run tests and print per-function coverage
	go test -race -coverprofile=coverage.out -covermode=atomic ./...
	go tool cover -func=coverage.out

lint: fmt vet staticcheck ## Run all checks CI runs

fmt: ## Fail if anything needs gofmt
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then echo "needs gofmt:"; echo "$$unformatted"; exit 1; fi

vet: ## go vet
	go vet ./...

staticcheck: ## staticcheck (installed on demand)
	@command -v staticcheck >/dev/null 2>&1 || go install honnef.co/go/tools/cmd/staticcheck@2025.1.1
	@staticcheck ./... || $$(go env GOPATH)/bin/staticcheck ./...

golden: ## Regenerate the golden HTML email files (review the diff!)
	go test ./internal/email -update

run: ## Run locally in dry_run mode against a temp database
	EMAIL_MODE=dry_run \
	POLL_TRIGGER_TOKEN=local-dev-token \
	DB_PATH=$${TMPDIR:-/tmp}/north-landing-badges.db \
	BASE_URL=http://localhost:8080 \
	go run ./cmd/server

docker: ## Build the deployable image
	docker build --build-arg VERSION=$(VERSION) -t north-landing-badges:$(VERSION) .

clean: ## Remove build output
	rm -rf bin coverage.out coverage.txt
