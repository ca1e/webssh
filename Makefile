BINARY := webssh
IMAGE := webssh:latest

.PHONY: help frontend-build helper build run dev-backend dev-frontend dev test integration test-ssh-server clean docker-build docker-up docker-down docker-logs

help:
	@echo "frontend-build     构建前端(npm install + vite build)"
	@echo "helper             交叉编译原始模式转发辅助程序(linux amd64/arm64,内嵌用)"
	@echo "build              构建前端 + 辅助程序 + 编译单二进制 $(BINARY)"
	@echo "run                构建并运行(默认 127.0.0.1:8090)"
	@echo "dev-backend        开发模式启动后端(go run,127.0.0.1:8090)"
	@echo "dev-frontend       开发模式启动前端(vite,:5174,/api 代理到 127.0.0.1:8090)"
	@echo "dev                同时启动前后端开发模式"
	@echo "test               单元测试(go test ./...)"
	@echo "integration        集成测试(自动拉起 cmd/testssh,需 go 工具链)"
	@echo "test-ssh-server    启动本地测试 SSH 服务端(:2222,用户 test/test123)"
	@echo "docker-build       构建 Docker 镜像 $(IMAGE)(三阶段:前端/Go/运行时)"
	@echo "docker-up          docker compose 构建并后台启动(宿主网络 :8090)"
	@echo "docker-down        docker compose 停止并移除容器"
	@echo "docker-logs        跟踪 compose 服务运行日志"
	@echo "clean              清理二进制与前端构建产物"

frontend-build:
	cd frontend && npm install && npm run build

# helper 交叉编译远端辅助程序(tools/socks5-helper,仅标准库)并压缩为
# 内嵌产物;产物随源码入库,使 go build/go test 开箱即用,修改 helper 后
# 重新执行本目标即可(内容寻址命名,远端旧版自动失效)。
helper:
	for arch in amd64 arm64; do \
		(cd tools/socks5-helper && GOOS=linux GOARCH=$$arch CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /tmp/socks5-helper-linux-$$arch .) && \
		gzip -9 -n -f -c /tmp/socks5-helper-linux-$$arch > internal/sshmgr/rawhelper/socks5-helper-linux-$$arch.gz; \
	done

build: frontend-build helper
	go build -trimpath -ldflags="-s -w" -o $(BINARY) .

run: build
	./$(BINARY)

dev-backend:
	WEBSSH_LISTEN_ADDR=:8090 go run .

dev-frontend:
	cd frontend && npm run dev

dev:
	$(MAKE) -j2 dev-backend dev-frontend

test:
	go test ./...

integration:
	go test -tags=integration ./internal/sshmgr/ ./internal/k8smgr/

test-ssh-server:
	go run ./cmd/testssh -addr :2222 -user test -pass test123

docker-build:
	docker build -t $(IMAGE) .

docker-up:
	docker compose up -d --build

docker-down:
	docker compose down

docker-logs:
	docker compose logs -f

clean:
	rm -f $(BINARY)
	rm -rf frontend/dist
