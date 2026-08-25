# 官方 #62 正例：正式 Makefile 有 GOOS=freebsd 目標（checker 應綠）
.PHONY: build build-linux build-freebsd build-all
LDFLAGS := -s -w
build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o gboard-node ./cmd/gboard-node
build-linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o gboard-node-linux-amd64 ./cmd/gboard-node
build-freebsd:
	CGO_ENABLED=0 GOOS=freebsd GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o gboard-node-freebsd-amd64 ./cmd/gboard-node
	CGO_ENABLED=0 GOOS=freebsd GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o gbctl-freebsd-amd64 ./cmd/gbctl
build-all: build-linux build-freebsd
