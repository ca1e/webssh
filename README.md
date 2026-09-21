# webssh

Web 前端 SSH 管理工具(Go + Vue3 单二进制部署)。

## 功能

- **主机管理**:保存连接(名称 / IP / 端口 / 用户名 / 认证类型 / 密码或私钥),支持密码与私钥(读取本地 `~/.ssh` 密钥列表)两种认证,可选经跳板机连接。主机信息保存在 XDG 配置目录(`$XDG_CONFIG_HOME/webssh/hosts.json`,默认 `~/.config/webssh/hosts.json`,权限 0600)。
- **终端**:多会话 Tab,基于 xterm.js 的完整终端(原始 PTY 字节流透传,支持完整转义协议 / 256 色 / 滚动回看),窗口尺寸自适应。主机可勾选 **Follow SSH path**:勾选后注入 OSC 7 提示符钩子(bash/zsh 无感注入,其他 shell 回退 type-ahead),SFTP 面板据此单向跟随终端 cd;该模式经 exec 启动 shell,不显示 Last login/MOTD 横幅。默认关闭 = 普通连接,无任何注入。**刷新恢复**:页面 F5 后已连接 Tab(含分屏结构、聚焦位置、各面板开关)按浏览器标签页自动重建(sessionStorage 布局记录 × 后端活会话求交),终端重挂为新 shell;新开标签页保持干净总览。**会话生命周期**:会话以"终端附着"为存活凭据,最后一个终端断开(关标签页/关浏览器/崩溃)后进入宽限期(`WEBSSH_ORPHAN_GRACE`,默认 5m),期内重连无缝,超期自动回收,不会永久占用连接与会话名额。
- **SFTP**:上下双文件树(上=远程主机,下=本机,下方也可切换为另一台已连接主机),显示名称 / 大小 / 所属用户 / 分组 / 权限;双击目录进入,当前目录文本框回车直达;支持选中文件的 上传 / 下载 / 删除(目录递归);目录不存在等错误有醒目提示。
- **统一传输**:本机↔远程、远程↔远程 全部走同一异步任务模型(`POST /api/transfers`):目录先枚举计划再由并发 worker(单任务 4,全局上限 8)拷贝,1MB 缓冲;进度经 `/api/transfers/ws` 推送,支持取消。任务仅存内存,进程重启即丢,无断点续传。
- **主机详情**:静态信息采集与实时指标(5s),Docker 容器列表 / 详情 / 日志流 / 容器内 shell;容器 ID 统一白名单清洗 + shell 转义。
- **代理 / 转发**:本地端口转发(`-L`)、SOCKS5 动态代理(`-D`),均只监听 127.0.0.1;跳板机在连接配置中指定。支持三种模式:**标准**(direct-tcpip / tcpip-forward)、**原始模式**(sshd `AllowTcpForwarding=no` 时的绕行方案):仅开一条 SSH exec 通道,在远端运行内嵌的辅助程序(静态编译,amd64/arm64,经 SFTP 或 base64 自动上传、内容寻址缓存、不监听任何端口),多条隧道连接在该通道上多路复用;「自动」模式先探测 sshd 配置,禁止时直接落原始模式,运行中被拒也会原地热切换。另支持 **K8s 服务端口转发**(OpenLens 式交互):选命名空间 → 列出 Service/Pod 及其端口 → 点击端口一键启动,本地端口自动分配,活动列表实时展示连接数与收发流量;亦支持手动添加(命名端口/其他资源类型/指定本地端口)。面向 sshd 禁止 TCP 转发(`AllowTcpForwarding=no`)但部署了 Kubernetes 的主机:流量经 SSH exec 通道在远端由 `kubectl port-forward` 桥接到 Service/Pod——远端只依赖 kubectl(无需 nc/socat),本机纯 Go 实现、跨平台零依赖。
- **K8s 面板**(借鉴 Freelens 的无 Agent 架构):工具栏打开,面向远端装有 kubectl 的主机。**集群状态**:单次 exec 探测当前 context / 集群名 / API Server / 服务端版本 / 节点就绪数,自动识别发行版(EKS/GKE/AKS/OpenShift/K3s/Kind/Minikube),结果按会话缓存(成功 60s / 失败 10s,支持强制刷新)。**Pod 列表**:经 `kubectl get pods -w --output-watch-events -o json` 的 WebSocket 事件流实时增量更新(List 快照全量替换 + ADDED/MODIFIED/DELETED 事件),支持全部命名空间(`-A`)或指定命名空间、名称过滤,断线 3s 自动重连并重新拉快照;kubectl 失败(RBAC/集群不可达)以错误事件透出。行操作:日志(`kubectl logs -f --tail N` 实时流,复用日志面板)与终端(`kubectl exec -it` 交互 PTY,仅 Running 的 Pod;bash 优先回退 sh,支持窗口 resize),以独立标签页打开、可刷新恢复。kubectl 定位统一走 PATH 直查 + 登录环境回退;动态参数一律 RFC 1123 白名单校验 + 单引号转义。
- **MCP Server**(AI Agent 接入,docs/mcp-design.md):把 SSH 管理能力封装为 **15 个工具**,经 Streamable HTTP 暴露在 `/mcp`,与 Web 端共享同一会话层/主机配置。核心不变式:**凭据只进不出**(Agent 可在新建主机时提交凭据明文,入库即加密;任何输出/日志永不回显);telnet 主机一律拒绝。`ssh_exec` 接受任意命令字符串(超时/输出上限可配)。**可选约束**(默认全关,Web 设置 → MCP 页管理,保存即热生效):**路径白名单**(exec 命令内路径与下载路径须落在前缀内,含符号链接逃逸防护)、**可见主机**(按分组/单台勾选,未勾选主机对 MCP 完全不可见)、**工具权限**(显式白名单,默认列出全部工具,清空即不暴露任何工具)。工具一览:

  | 工具 | 域 | 说明 |
  | --- | --- | --- |
  | `ssh_list_hosts` | hosts | 主机元数据(脱敏投影,无凭据) |
  | `ssh_host_info` | hosts | 单主机配置详情(同上口径) |
  | `ssh_test_connection` | hosts | 拨号+认证+host key 校验,不留连接 |
  | `ssh_create_host` | hosts | 新建 SSH 主机(口令/私钥认证;凭据入参即加密,输出脱敏;可见主机开关开启时自动入名单) |
  | `ssh_duplicate_host` | hosts | 复制主机(凭据引用与源共享;名称可选;副本自动入可见名单) |
  | `ssh_delete_host` | hosts | 删除主机(confirm=true 二次确认;被用作跳板时拒删) |
  | `ssh_exec` | exec | 任意命令执行,返回 stdout/stderr/exit_code(唯一执行入口) |
  | `ssh_system_info` | monitor | 静态主机信息 |
  | `ssh_cpu_memory` | monitor | CPU/内存采样 |
  | `ssh_disk_usage` | monitor | df 解析 |
  | `ssh_network_status` | monitor | 网络吞吐采样 + 接口地址 |
  | `ssh_service_status` | monitor | systemd 单元状态(只读) |
  | `ssh_docker_status` | monitor | 容器列表 + 引擎/Compose 信息 |
  | `ssh_k8s_status` | monitor | 集群探测(缓存) |
  | `ssh_download_file` | files | SFTP → 服务端暂存目录,返回路径/sha256/大小 |

  **接入**:`WEBSSH_MCP_TOKEN=<口令>`(或 `$XDG_CONFIG_HOME/webssh/mcp.json` 的 `token` 字段,环境变量优先)。未配置 = MCP 整体禁用(`/mcp` 返回 404)。请求带 `Authorization: Bearer <口令>`,Claude Desktop(mcp-remote) 配置示例:

  ```json
  {
    "mcpServers": {
      "webssh": {
        "url": "http://127.0.0.1:8090/mcp",
        "headers": { "Authorization": "Bearer <你的口令>" }
      }
    }
  }
  ```

  可选策略文件 `$XDG_CONFIG_HOME/webssh/mcp.json`(也可在 Web 设置 → MCP 页管理路径白名单;`SIGHUP` 热重载):

  ```json
  {
    "enabled": true,
    "domains": { "hosts": true, "exec": true, "files": true, "monitor": true },
    "defaults": {
      "exec_timeout": "15s", "read_max_bytes": 1048576, "download_max_bytes": 104857600,
      "path_whitelist": { "enabled": false, "paths": ["/var/log/"] }
    },
    "visible_hosts": { "enabled": false, "ids": ["<host_id>", "..."] },
    "tool_filter": { "tools": ["ssh_exec", "..."] }
  }
  ```

## 快速开始

```bash
cd webssh
make run          # 构建前端+后端,启动 http://127.0.0.1:8090
```

开发模式(前后端分离,前端热更新):

```bash
make dev          # 等价于同时启动 dev-backend(127.0.0.1:8090) 与 dev-frontend(:5174)
```

Docker Compose 部署(单容器,宿主网络):

```bash
docker compose up -d --build   # 构建镜像并启动,服务监听宿主机 :8090
```

- **配置持久化**:`hosts.json` / `known_hosts.json` / 锁屏密码落在 named volume `webssh-config`(容器内 `/home/webssh/.config`);查看/备份用 `docker compose exec webssh cat /home/webssh/.config/webssh/hosts.json` 或 `docker volume inspect webssh_webssh-config`。
- **本机私钥**:默认把宿主 `~/.ssh` 只读挂进容器,供"本地私钥"认证方式选用;宿主用户 UID 非 1000 时用 `docker compose build --build-arg WEBSSH_UID=<uid>` 对齐,或改用 `WEBSSH_SSH_KEYS_DIR` 挂载其他路径。
- **网络**:`network_mode: host`,监听 `:8090` 即绑定所有网卡、局域网可达——容器化不改变"API 无独立鉴权"的前提,**务必先在设置里设置锁屏密码**;只想本机访问可把 `WEBSSH_LISTEN_ADDR` 改回 `127.0.0.1:8090`。
- 其他环境变量(`WEBSSH_MAX_SESSIONS` / `WEBSSH_ORPHAN_GRACE`)直接加在 compose 的 `environment:` 里。

本机没有 sshd 时,可用自带的测试服务端联调(用户 test / 密码 test123):

```bash
make test-ssh-server
```

> 注意:testssh 的 SFTP 直接访问真实文件系统,没有 chroot,测试请约定在临时目录内进行。

## 测试

```bash
make test          # 单元测试(解析器、同源校验、传输引擎等,表驱动)
make integration   # 集成测试:自动编译并拉起 cmd/testssh,覆盖
                   # 连接/终端/OSC 7 cwd 跟踪/启动命令/SFTP/会话上限
```

## 配置(环境变量)

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `WEBSSH_LISTEN_ADDR` | `127.0.0.1:8090` | HTTP 监听地址 |
| `WEBSSH_MAX_SESSIONS` | `64` | 同时活动会话数量上限 |
| `WEBSSH_ORPHAN_GRACE` | `5m` | 最后一个终端断开后到自动回收孤儿会话之间的宽限期(支持 `30s`/`1h` 等时长写法;解析失败回落默认值) |
| `WEBSSH_SSH_KEYS_DIR` | `~/.ssh` | 本地私钥目录 |
| `WEBSSH_MCP_TOKEN` | (空) | MCP Bearer 口令(优先于 `mcp.json` 的 `token` 字段);两者均未配置时**自动复用锁屏密码**;三处均无时 MCP 整体禁用(`/mcp` 404)。改锁屏密码后 MCP 认证立即跟随 |
| `WEBSSH_MCP_ENABLED` | `true` | 设为 `false` 强制禁用 MCP(`/mcp` 404) |
| `WEBSSH_MASTER_KEY` | (空) | base64 的 32B 主密钥,覆盖 `master.key` 文件(容器从 secret 注入) |

## 安全说明

- **默认仅监听 127.0.0.1**:全部 API 无鉴权,服务必须只暴露给本机。如确需外部访问,请经反向代理(TLS 终结 + 鉴权)转发,不要直接改 `WEBSSH_LISTEN_ADDR` 为 `0.0.0.0`。
- **WebSocket 同源校验**:所有 WS 端点校验 Origin 与 Host 一致(非浏览器客户端无 Origin 放行),防御恶意网页对 127.0.0.1 服务的跨站 WebSocket 劫持。
- **凭据加密存储**:主机密码/私钥口令以 AES-256-GCM 加密存于 `secrets.json`(0600),主密钥在 `master.key`(32B 随机,0600,首启自动生成;可用 `WEBSSH_MASTER_KEY` 环境变量注入 base64 密钥供容器场景使用);`hosts.json` 只存凭据引用,无任何明文/base64 凭据。**主机迁移/备份**用导出功能:加密导出(bundle,口令经 scrypt 派生密钥逐条加密)或无密码导出(明文凭据,仅限内网备份/快速迁移,文件名 `hosts-plaintext.json`);导入仅接受 WebSSH 导出的 bundle 格式。
- **MCP 接入安全**:口令正确即拥有全部只读工具与全部主机访问权,请使用强口令并通过反向代理(TLS)暴露;服务默认仅监听 127.0.0.1。每次工具调用留痕于服务日志(工具/主机/结果/耗时)。`mcp-downloads/` 暂存目录位于配置目录内,注意磁盘占用与清理。
- SSH 主机密钥采用 TOFU(首次信任)策略,指纹记录在 `known_hosts.json`;指纹变化时连接会失败,确认安全后重新连接即可。
- 远程 shell 工作目录跟随(SFTP 面板)仅在主机勾选 Follow SSH path 时启用,依赖注入的 OSC 7 提示符钩子(bash/zsh 无感注入,其他 shell 静默跳过);fish 等 shell 下 SFTP 面板不会自动跟随终端目录,属预期行为。
