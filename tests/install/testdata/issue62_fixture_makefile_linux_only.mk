# 官方 #62 反例：正式 Makefile 只有 linux，沒有 GOOS=freebsd（測必須紅）
.PHONY: build build-linux build-all
LDFLAGS := -s -w
build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o gboard-node ./cmd/gboard-node
build-linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o gboard-node-linux-amd64 ./cmd/gboard-node
build-all: build-linux
