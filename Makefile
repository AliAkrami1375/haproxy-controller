# HAProxy Controller
# Powered by Ebdaa.me - https://ebdaa.me

BINARY      := haproxy-controller
PKG         := ./cmd/haproxy-controller
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS     := -s -w -X main.version=$(VERSION)
GOFLAGS     := -trimpath

.DEFAULT_GOAL := build

.PHONY: build
build: ## Build the static controller binary into bin/
	@mkdir -p bin
	CGO_ENABLED=0 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o bin/$(BINARY) $(PKG)
	@echo "built bin/$(BINARY) ($(VERSION))"

.PHONY: run
run: build ## Build and run against ./dev (a throwaway local tree)
	@mkdir -p dev/etc/errors dev/etc/certs dev/data
	./bin/$(BINARY) -config dev/controller.json

.PHONY: test
test: ## Run the test suite
	go test ./... -count=1

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: fmt
fmt: ## Format all Go source
	gofmt -w cmd internal

.PHONY: check
check: fmt vet test ## Format, vet and test

.PHONY: haproxy
haproxy: ## Build and install the vendored HAProxy (needs root)
	sudo ./scripts/build-haproxy.sh

.PHONY: install
install: build ## Install the controller and its systemd services (needs root)
	sudo ./scripts/install.sh

.PHONY: docker
docker: ## Build the self-contained Docker image
	docker build -t haproxy-controller:latest .

.PHONY: vendor-haproxy
vendor-haproxy: ## Re-vendor third_party/haproxy (only when changing HAProxy release)
	./scripts/vendor-haproxy.sh

.PHONY: docker-run
docker-run: docker ## Build and run the image with persistent volumes
	docker run -d --name haproxy-controller \
	  -p 127.0.0.1:9000:9000 -p 80:80 -p 443:443 \
	  -v hc-data:/var/lib/haproxy-controller \
	  -v hc-etc:/etc/haproxy \
	  -v hc-conf:/etc/haproxy-controller \
	  haproxy-controller:latest
	@echo "Panel: http://127.0.0.1:9000/  (password: docker logs haproxy-controller)"

.PHONY: clean
clean: ## Remove build output
	rm -rf bin dev/data dev/etc

.PHONY: help
help: ## List available targets
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
	  | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'
