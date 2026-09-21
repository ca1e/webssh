# WebSSH MCP Server 实现说明

把 WebSSH 的 SSH 管理能力封装为 MCP（Model Context Protocol）工具集，供 AI Agent 执行远程运维。
核心不变式：**Agent 永不接触密码/私钥**；telnet 主机一律拒绝。

---

## 1. 架构

```
AI Agent ──Bearer──► /mcp (Streamable HTTP, go-sdk)
                          │
Web 浏览器 ──────────► /api/** (handler)
                          │
                          ▼
                 sshmgr.Manager ◄── 共享会话注册表
```

- **同进程挂载**：MCP Server 作为 gin 路由 `/mcp`，与 Web API 共享 `sshmgr.Manager` / `store` / SFTP 通道，直接复用同一会话（无重复拨号）。
- **SDK**：`github.com/modelcontextprotocol/go-sdk/mcp`。装配面 `mcp.NewServer` + `mcp.AddTool`（handler 签名 `func(ctx, *mcp.CallToolRequest, In) (*mcp.CallToolResult, Out, error)`，自动绑定 JSON Schema）；HTTP 面 `mcp.NewStreamableHTTPHandler`（Stateless + JSONResponse）；测试面 `mcp.NewInMemoryTransports`。
- **凭据零暴露**：所有工具输出统一走 `store.PublicHost` / `mcpHostView` 脱敏投影——凭据字段不出现，auth 类型仅以无敏感词形的标识 + `Has*` 布尔回答；集成测试对全部工具输出做凭据模式快照断言。

代码布局：

```
internal/mcp/
    server.go          # 装配、Bearer 中间件、主机解析、工具注册
    config.go          # mcp.json 加载与校验
    settings.go        # 设置读写（Web 设置页 /api/mcp/settings 的后端）
    auth.go            # 预共享口令校验（ConstantTimeCompare / bcrypt）
    pathguard/         # 可选路径白名单 + 符号链接逃逸防护
    tools_hosts.go     # ssh_list_hosts / ssh_host_info / ssh_test_connection
    tools_exec.go      # ssh_exec
    tools_files.go     # ssh_download_file
    tools_monitor.go   # ssh_system_info / ssh_cpu_memory / ssh_disk_usage / ssh_network_status / ssh_service_status / ssh_docker_status / ssh_k8s_status
```

---

## 2. 认证

- 预共享口令 Bearer：`Authorization: Bearer <口令>`。口令来源优先级：环境变量 `WEBSSH_MCP_TOKEN` > `mcp.json` 的 `token` 字段 > 锁屏密码（bcrypt 逐请求校验）；显式配置的 token 走 ConstantTimeCompare。
- 未配置任何口令 = MCP 整体禁用（fail-closed，`/mcp` 返回 404 而非 401，不暴露服务存在性）。锁屏密码后设/更换立即生效，无需重启。
- 每次工具调用留痕于服务日志：工具 / 主机 / 结果 / 耗时（`callLog`）。

---

## 3. 工具面（12 个）

| 工具 | 域 | 说明 |
| --- | --- | --- |
| `ssh_list_hosts` | hosts | 主机元数据(脱敏投影，无凭据；受可见主机开关过滤) |
| `ssh_host_info` | hosts | 单主机配置详情(同上口径) |
| `ssh_test_connection` | hosts | 拨号+认证+host key 校验，不留连接 |
| `ssh_exec` | exec | 执行任意命令字符串，返回 `{stdout, stderr, exit_code, truncated}` |
| `ssh_system_info` | monitor | 静态主机信息 |
| `ssh_cpu_memory` | monitor | CPU/内存/负载采样(1 秒差分) |
| `ssh_disk_usage` | monitor | df 解析(排除伪文件系统) |
| `ssh_network_status` | monitor | 网络吞吐采样 + 接口地址 |
| `ssh_service_status` | monitor | systemd 单元只读状态 |
| `ssh_docker_status` | monitor | 容器列表 + 引擎/Compose 信息 |
| `ssh_k8s_status` | monitor | 集群探测(缓存) |
| `ssh_download_file` | files | SFTP → 服务端暂存目录，返回路径/sha256/大小 |

**显式不做**：审计日志、人工审批（elicitation）、会话管理工具、脚本执行层、远程文件写/删/读、上传、进程管理、服务启停、批量编排、telnet 暴露、命令配额、OAuth、多租户。

---

## 4. 执行（ssh_exec）

- 入参 `host` + `command`（任意命令字符串），经 `Session.RunExec` 分离式 exec 通道执行；`~`/环境不展开，行为等同登录 shell 的 `ssh host "command"`。
- 超时：`defaults.exec_timeout`（默认 15s），超时中止并返回可纠偏文案。
- 输出上限：`defaults.read_max_bytes`（默认 1 MiB/流），超限置 `truncated` 并附提示。
- 路径白名单开启时（见「可选约束」一节）：提取命令中的路径形 token（按空白与 shell 分隔符切分，取 `/`、`~` 开头者），逐个经 pathguard 校验，任一越界即拒绝整条命令。

---

## 5. 文件下载（ssh_download_file）

- 入参 `host` + `path`（绝对路径或 `~/` 开头；`~/` 展开为远端主目录）。
- SFTP 流式下载到服务端暂存目录（`$ConfigDir/mcp-downloads/`，0700），返回 `{local_path, remote_path, sha256, size}`；不整读进内存，流式写盘同时计数。
- 大小上限 `defaults.download_max_bytes`（默认 100 MiB），超限即中止并清理暂存文件。

---

## 6. 可选约束（默认全部关闭）

| 配置 | 行为 |
| --- | --- |
| `defaults.path_whitelist` | 开启后：`ssh_exec` 命令内路径与 `ssh_download_file` 的 path 须落在前缀内。服务侧统一做 `~/` 展开、`path.Clean`、前缀匹配与逐级符号链接解析（SFTP Lstat/ReadLink，16 跳防环）；拒绝文案携带当前生效白名单，Agent 可自行纠偏。 |
| `visible_hosts` | 开启后：`ssh_list_hosts` 仅返回 `ids` 内的主机；其余主机在任何工具中按「不存在」处理（不泄露存在性）。 |
| `tool_filter` | 显式工具白名单：仅 `tools` 内的工具注册暴露（`tools/list` 与调用双重生效）；空列表 = 不暴露任何工具；默认配置显式列出全部 12 个工具。工具名在加载与保存时校验，未知工具直接拒绝。 |

三段配置均可在 Web 设置 → MCP 页管理，保存即写 `mcp.json` 并热重载生效。

---

## 7. 配置文件（mcp.json）

可选文件 `$XDG_CONFIG_HOME/webssh/mcp.json`；不存在 = 全默认。`SIGHUP` 热重载（失败保持旧配置并告警）。加载用 `DisallowUnknownFields`，拼错字段名直接拒绝启动。

```json
{
  "enabled": true,
  "token": "",
  "domains": { "hosts": true, "exec": true, "files": true, "monitor": true },
  "defaults": {
    "exec_timeout": "15s",
    "read_max_bytes": 1048576,
    "download_max_bytes": 104857600,
    "path_whitelist": { "enabled": false, "paths": ["/var/log/"] }
  },
  "visible_hosts": { "enabled": false, "ids": ["<host_id>"] },
  "tool_filter": { "tools": ["ssh_exec"] }
}
```

设置页持久化（`Server.UpdateSettings`）以 map 合并方式只覆盖 `path_whitelist` / `visible_hosts` / `tool_filter` 三段，其余字段原样保留；写盘成功但热重载失败时返回错误，内存保持旧策略。

---

## 8. 测试

- 单元：配置加载/拒绝矩阵；pathguard 前缀匹配、`~/` 展开、`..` 规范化；可见主机过滤；工具过滤；设置读写回环（持久化保留既有字段 + 热生效 + guard 同步）。
- 集成（`-tags=integration`）：拉起 `cmd/testssh`，经 go-sdk InMemory transports 对 12 个工具逐个走 happy 路径（真会话/真 exec/真 SFTP），并对全部输出做凭据零暴露快照断言。
- 协议层：tools/list 数量、deny 路径（未知主机/telnet/空命令/相对路径）返回 IsError 文本而非协议错误、无堆栈、无凭据词形。
