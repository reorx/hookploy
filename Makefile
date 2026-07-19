VERSION ?= $(shell git describe --tags --always --dirty)
LDFLAGS  = -s -w -X github.com/reorx/hookploy/internal/version.Version=$(VERSION)
GOBUILD  = CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)"

.PHONY: test build build-linux-amd64 build-darwin-arm64 dist clean

test:
	go test ./...

# 本机平台构建（开发调试用）
build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o tmp/hookploy ./cmd/hookploy

# 生产部署产物：deploy 仓库 ansible role 从 dist/ 上传
build-linux-amd64:
	GOOS=linux GOARCH=amd64 $(GOBUILD) -o dist/hookploy-linux-amd64 ./cmd/hookploy

build-darwin-arm64:
	GOOS=darwin GOARCH=arm64 $(GOBUILD) -o dist/hookploy-darwin-arm64 ./cmd/hookploy

dist: build-linux-amd64 build-darwin-arm64

clean:
	rm -rf dist tmp/hookploy tmp/hookploy-*
