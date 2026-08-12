# syntax=docker/dockerfile:1
# ============================================================================
# triproxy 多阶段构建
#   构建阶段：Go 1.25 静态编译（CGO_ENABLED=0，支持 --platform=linux/arm64）
#   运行阶段：alpine 最小运行时（含 CA 证书，支持 https 上游 + wget 健康检查），
#             以非 root 用户运行
#
# 构建：docker build -t triproxy:latest .
#       交叉架构：docker build --platform=linux/arm64 -t triproxy:arm64 .
# 运行：docker run -d -p 8866:8866 \
#         -v /path/to/config.yaml:/etc/triproxy/config.yaml triproxy:latest
# ============================================================================

# ---------- 构建阶段 ----------
FROM golang:1.25-alpine AS builder
ARG TARGETARCH
ARG TARGETVARIANT
# Go 模块代理：国内网络默认 goproxy.cn，构建时可覆盖：
#   docker build --build-arg GOPROXY=https://proxy.golang.org,direct -t triproxy .
ARG GOPROXY=https://goproxy.cn,direct
WORKDIR /src

# 先拷贝依赖清单，利用 Docker 层缓存避免每次重复下载
COPY go.mod go.sum ./
RUN GOPROXY=${GOPROXY} go mod download

COPY . .
# -trimpath 去除构建路径，-s -w 精简二进制体积
# TARGETARCH/TARGETVARIANT 由 buildx 自动注入（如 arm64 / amd64 / arm+v7）；
# GOARM 只在 arm 变体时设置（TARGETVARIANT 形如 v7）
RUN if [ -n "${TARGETVARIANT}" ]; then export GOARM="${TARGETVARIANT#v}"; fi \
    && CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH:-amd64} \
       go build -trimpath -ldflags="-s -w" -o /out/triproxy .

# ---------- 运行阶段 ----------
FROM alpine:3.20

# ca-certificates：上游可能是 https，需要系统根证书
# wget：用于 HEALTHCHECK 探测 /healthz
RUN apk add --no-cache ca-certificates wget \
    && addgroup -S triproxy \
    && adduser -S -G triproxy triproxy \
    && mkdir -p /etc/triproxy \
    && chown -R triproxy:triproxy /etc/triproxy

COPY --from=builder /out/triproxy /usr/local/bin/triproxy
# 默认配置为示例配置；部署时务必用 -v 挂载真实 config.yaml 覆盖
COPY --chown=triproxy:triproxy config.example.yaml /etc/triproxy/config.yaml

USER triproxy
EXPOSE 8866

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -q -O /dev/null http://127.0.0.1:8866/healthz || exit 1

ENTRYPOINT ["triproxy", "-config", "/etc/triproxy/config.yaml"]
