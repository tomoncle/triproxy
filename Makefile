# ============================================================================
# triproxy Makefile
#
# 常用目标：
#   make build      本地编译出 ./triproxy
#   make test       跑单元测试
#   make dist       交叉编译 5 个平台二进制到 dist/（供 GitLab Release 发布）
#   make docker     构建 Docker 镜像
#   make clean      清理构建产物
# ============================================================================

BINARY     := triproxy
DIST_DIR   := dist
GO         ?= go
DOCKER     ?= docker
IMAGE      ?= triproxy
IMAGE_TAG  ?= latest

# 与 GitHub Release 发布产物保持一致的平台列表
# 覆盖：Linux x86/ARM/32位/RISC-V/龙芯/POWER/IBM Z，macOS，Windows，FreeBSD
PLATFORMS  := linux/amd64 linux/arm64 linux/386 linux/arm linux/riscv64 \
              linux/loong64 linux/ppc64le linux/s390x \
              darwin/amd64 darwin/arm64 \
              windows/amd64 windows/arm64 windows/386 \
              freebsd/amd64 freebsd/arm64

BUILD_FLAGS := -trimpath -ldflags "-s -w"

.PHONY: all build test vet fmt clean dist docker

all: build

## 本地编译
build:
	$(GO) build $(BUILD_FLAGS) -o $(BINARY) .

test:
	$(GO) test -count=1 ./...

vet:
	$(GO) vet ./...

fmt:
	gofmt -l -w .

## 交叉编译 dist/ 下的全平台二进制
dist:
	@mkdir -p $(DIST_DIR)
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		name=$(BINARY)-$$os-$$arch; \
		if [ "$$os" = "windows" ]; then name=$$name.exe; fi; \
		echo "==> $(BINARY) $$p"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build $(BUILD_FLAGS) -o $(DIST_DIR)/$$name . || exit 1; \
	done
	@echo "dist 完成: $(DIST_DIR)/"

## Docker 镜像
docker:
	$(DOCKER) build -t $(IMAGE):$(IMAGE_TAG) .

## Docker 多架构镜像（buildx，含 amd64/arm64/arm/v7/386/ppc64le/s390x/riscv64）
docker-multi:
	$(DOCKER) buildx build \
		--platform linux/amd64,linux/arm64,linux/arm/v7,linux/386,linux/ppc64le,linux/s390x,linux/riscv64 \
		-t $(IMAGE):$(IMAGE_TAG) --push .

clean:
	rm -f $(BINARY)
	rm -rf $(DIST_DIR)
