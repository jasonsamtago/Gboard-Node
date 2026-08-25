# 官方 #48 邊界：開發用 go test／未 strip 的本機 debug build 不回歸
# 測不要強迫 test binary 也 strip。
test:
	go test -v -race -count=1 ./internal/...
debug:
	go build -o gboard-node ./cmd/gboard-node
