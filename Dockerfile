# cf-probe 运行镜像:拷贝 CI/本地预编译的静态二进制(与 komari-agent 同构)。
# 编译不在镜像内进行,由 .github/workflows/release-docker.yml 或本地 go build 产出 dist/。
#
# 本地构建示例:
#   CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
#     -ldflags "-s -w -X main.version=dev" -o dist/cf-probe-linux-amd64 ./cmd/cf-probe
#   docker build -t cfsm-agent:local .
#
# 多架构由 buildx 自动填充 TARGETOS / TARGETARCH。
FROM alpine:3.21

WORKDIR /app

ARG TARGETOS
ARG TARGETARCH

# HTTPS 上报/WSS 与更新检查需要 CA 证书;默认 tcp ping 模式无需额外能力,
# 若使用 ICMP 探测模式请在运行时追加 --cap-add=NET_RAW。
RUN apk add --no-cache ca-certificates

COPY --chmod=755 dist/cf-probe-${TARGETOS}-${TARGETARCH} /app/cf-probe
COPY --chmod=755 entrypoint.sh /entrypoint.sh

# 配置与月流量状态(traffic.dat)持久化目录,运行时用 -v 挂载。
RUN mkdir -p /data
VOLUME /data

# 仅作镜像标识,便于人工识别;容器逻辑不依赖此文件。
RUN touch /.cf-probe-container

ENTRYPOINT ["/entrypoint.sh"]
