BINARY := webssh
IMAGE := webssh:latest

.PHONY: help frontend-build build run dev-backend dev-frontend dev test integration test-ssh-server clean docker-build docker-up docker-down docker-logs

help:
	@echo "frontend-build     构建前端(npm install + vite build)"
	@echo "build              构建前端 + 编译单二进制 $(BINARY)"
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

build: frontend-build
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
