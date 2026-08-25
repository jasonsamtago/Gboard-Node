# 官方 #48 反例：正式 Makefile build 漏 -s -w（測必須紅）
# testdata 寫明：現 tip 已有 -s -w 時，拿掉仍必須被本測打紅。
LDFLAGS := -X main.version=dev
build:
	go build -ldflags "$(LDFLAGS)" -o gboard-node ./cmd/gboard-node
