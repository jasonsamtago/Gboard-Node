VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
BUILD_TIME ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
ARCH ?= amd64
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.buildTime=$(BUILD_TIME) -X main.commit=$(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)

.PHONY: build clean test docker install build-linux build-linux-arm64 build-freebsd build-all

# Build for current platform
build:
	go build -trimpath -ldflags "$(LDFLAGS)" -tags "with_quic with_utls with_wireguard with_clash_api" -o gboard-node ./cmd/gboard-node
	go build -trimpath -ldflags "$(LDFLAGS)" -o gbctl ./cmd/gbctl

# Build for Linux amd64
build-linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -tags "with_quic with_utls with_wireguard with_acme with_clash_api" -o gboard-node-linux-amd64 ./cmd/gboard-node
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o gbctl-linux-amd64 ./cmd/gbctl

# Build for Linux arm64
build-linux-arm64:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -tags "with_quic with_utls with_wireguard with_acme with_clash_api" -o gboard-node-linux-arm64 ./cmd/gboard-node
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o gbctl-linux-arm64 ./cmd/gbctl

# Build for FreeBSD (override with ARCH=arm64). Same formal -trimpath -ldflags -s -w as Linux.
# Not added to build-all / CI matrix — official #62 is compile target + install.sh only.
build-freebsd:
	CGO_ENABLED=0 GOOS=freebsd GOARCH=$(ARCH) go build -trimpath -ldflags "$(LDFLAGS)" -tags "with_quic with_utls with_wireguard with_acme with_clash_api" -o gboard-node-freebsd-$(ARCH) ./cmd/gboard-node
	CGO_ENABLED=0 GOOS=freebsd GOARCH=$(ARCH) go build -trimpath -ldflags "$(LDFLAGS)" -o gbctl-freebsd-$(ARCH) ./cmd/gbctl

# Build all platforms
build-all: build-linux build-linux-arm64

# Run tests
test:
	go test -v -race -count=1 ./internal/... ./cmd/gbctl ./tests/install

# Clean build artifacts
clean:
	rm -f gboard-node gbctl gboard-node-linux-* gbctl-linux-* gboard-node-freebsd-* gbctl-freebsd-*

# Build Docker image
docker:
	docker build -t gboard-node:$(VERSION) -t gboard-node:latest .

# Install to system (single node, legacy compat)
install: build
	sudo cp gboard-node /usr/local/bin/
	sudo cp gbctl /usr/local/bin/
	sudo mkdir -p /etc/gboard-node
	@if [ ! -f /etc/gboard-node/config.yml ]; then \
		sudo cp config.yml.example /etc/gboard-node/config.yml; \
		echo "Config copied to /etc/gboard-node/config.yml - please edit it"; \
	fi
