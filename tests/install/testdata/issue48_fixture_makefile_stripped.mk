# 官方 #48 正例：正式 build 已帶 -trimpath -ldflags="-s -w"
# 用來證明 checker 能綠；從此檔拿掉 -s -w 必須再紅（回歸鎖）。
LDFLAGS := -s -w -X main.version=dev
build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o gboard-node ./cmd/gboard-node
