# 四个 Go 服务共用的镜像定义（用 --build-arg BIN=api|worker|sandboxd|gateway 复用）
#
# 目标：镜像尽量小、只含一个静态二进制。
# distroless/static 已自带 ca-certificates 与 tzdata，不需要额外装。
#
# 注意 readOnlyRootFilesystem: true（见 k8s 清单）要求程序不往磁盘写东西，
# 临时文件统一走 /tmp（已挂 emptyDir）。如果某个服务要写状态文件，改挂 volume，
# 别关 readOnlyRootFilesystem —— 那是最便宜的一道防线。

FROM golang:1.24-bookworm AS build
ARG BIN
ARG VERSION=dev
ARG COMMIT=unknown

WORKDIR /src
# 先拷依赖清单，利用层缓存（改代码不重下依赖）
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=0 → 静态链接，可以进 distroless/static
# -trimpath 去掉构建机路径（避免泄露内网目录结构）
# -s -w 去符号表和调试信息
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath \
      -ldflags="-s -w \
        -X main.version=${VERSION} \
        -X main.commit=${COMMIT}" \
      -o /out/app ./cmd/${BIN}

# ---------------------------------------------------------------- 运行镜像
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/app /app/server

USER nonroot:nonroot
EXPOSE 8080 9090
ENTRYPOINT ["/app/server"]
