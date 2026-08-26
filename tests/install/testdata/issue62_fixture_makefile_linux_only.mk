# 官方 #62 反例：只有 build-linux*，沒有 make build-freebsd
.PHONY: build-linux build-linux-arm64
LDFLAGS := -s -w
build-linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o gboard-node-linux-amd64 ./cmd/gboard-node
build-linux-arm64:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o gboard-node-linux-arm64 ./cmd/gboard-node
