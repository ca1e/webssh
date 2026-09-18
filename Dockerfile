# syntax=docker/dockerfile:1
# ---------- 阶段 1:构建前端(npm ci + vite build) ----------
FROM node:22-alpine AS frontend
WORKDIR /fe
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci
COPY frontend/ ./
RUN npm run build

# ---------- 阶段 2:编译 Go 单二进制(go:embed 打入 frontend/dist) ----------
FROM golang:1.25-alpine AS build
# Go 模块代理(默认国内镜像;海外构建可 --build-arg GOPROXY=https://proxy.golang.org,direct)
ARG GOPROXY=https://goproxy.cn,direct
ENV GOPROXY=${GOPROXY}
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=frontend /fe/dist ./frontend/dist
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/webssh .

# ---------- 阶段 3:运行时(纯 Go,无外部命令依赖) ----------
FROM alpine:3.20
# 运行用户 UID:用命名 volume 时数据目录属主自动随镜像;换 bind mount 需与宿主目录属主一致
ARG WEBSSH_UID=1000
RUN adduser -D -u ${WEBSSH_UID} -h /home/webssh webssh \
    && mkdir -p /home/webssh/.config/webssh /home/webssh/.ssh \
    && chown -R webssh:webssh /home/webssh
COPY --from=build /out/webssh /usr/local/bin/webssh
USER webssh
ENV XDG_CONFIG_HOME=/home/webssh/.config
VOLUME ["/home/webssh/.config"]
EXPOSE 8090
ENTRYPOINT ["/usr/local/bin/webssh"]
