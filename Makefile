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

# 发布产物：与 .github/workflows/release.yml 同形态的 tar.gz（binary +
# hookploy-ctl.sh）+ checksums.txt，使 release 可本地复现。
# 同时保留 dist/hookploy-<os>-<arch> 裸 binary，deploy 仓库的 ansible role 从那里上传。
SHA256 = $(shell command -v sha256sum >/dev/null 2>&1 && echo sha256sum || echo 'shasum -a 256')

dist: build-linux-amd64 build-darwin-arm64
	@set -eu; \
	rm -f dist/*.tar.gz dist/checksums.txt; \
	for target in darwin/arm64 linux/amd64; do \
	  os="$${target%/*}"; arch="$${target#*/}"; \
	  outdir="dist/hookploy-$(VERSION)-$${os}-$${arch}"; \
	  rm -rf "$$outdir"; mkdir -p "$$outdir"; \
	  install -m 0755 "dist/hookploy-$${os}-$${arch}" "$$outdir/hookploy"; \
	  install -m 0755 scripts/hookploy-ctl.sh "$$outdir/hookploy-ctl.sh"; \
	  tar -C dist -czf "$${outdir}.tar.gz" "$$(basename "$$outdir")"; \
	  rm -rf "$$outdir"; \
	  echo "packed $${outdir}.tar.gz"; \
	done; \
	cd dist && $(SHA256) ./*.tar.gz > checksums.txt

clean:
	rm -rf dist tmp/hookploy tmp/hookploy-*
