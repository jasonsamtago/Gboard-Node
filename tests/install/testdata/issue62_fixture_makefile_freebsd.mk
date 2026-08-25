# 官方 #62 正例：make build-freebsd 產出 gboard-node-freebsd-${ARCH}
ARCH ?= amd64
LDFLAGS := -s -w
.PHONY: build-freebsd
build-freebsd:
	CGO_ENABLED=0 GOOS=freebsd GOARCH=$(ARCH) go build -trimpath -ldflags "$(LDFLAGS)" -o gboard-node-freebsd-$(ARCH) ./cmd/gboard-node
