# 原始模式转发(SaSSHimi 模式)设计与部署

## 背景与原理

目标主机 sshd 配置 `AllowTcpForwarding=no`(或 `DisableForwarding=yes`)时,
标准本地转发(direct-tcpip)与 SOCKS5(tcpip-forward)的通道开启请求会被
sshd 以 `administratively prohibited` 拒绝。

原始模式不复用任何转发通道,只用一条普通 SSH **exec session**:

```
┌─────────────── webssh 后端(本地) ───────────────┐   ┌────── 远端主机 ──────┐
│ :1080 SOCKS5 ─┐                                 │   │                      │
│ :8080 本地转发 ─→ RawForwarder(帧多路复用) ──── SSH exec ──→ socks5-helper │
│               │   connID→net.Conn 映射          │   │  ├─ TCP → host:port  │
└───────────────┴─────────────────────────────────┘   │  └─ 只用 stdin/stdout│
                                                       └──────────────────────┘
```

- 后端把多条隧道连接封装为二进制帧写入 helper 的 stdin,从 stdout 读回帧
  并按连接 ID 分发到对应 `net.Conn`(见 `internal/sshmgr/raw_forwarder.go`)。
- helper(见 `tools/socks5-helper`,仅标准库、静态编译)解析帧、真实拨号、
  回传数据;不监听任何网络端口,`exec` 替换登录 shell 自身。
- 全程只有 session 通道,转发禁令无从生效。

### 帧协议(整数大端)

```
+---------+--------+--------+---------+
| connID  |  type  | length | payload |
| 4 字节  | 1 字节 | 4 字节 | length  |
+---------+--------+--------+---------+

1=OPEN(host:port)  本地→远端,拨号请求
5=ACK(空)          远端→本地,拨号成功应答(SOCKS5 回包前必须得知成败,
                    HTTP 等目标不会先发数据,无法以首包推断,故为显式应答)
4=ERROR(message)   远端→本地,拨号失败原因
2=DATA             双向,原始字节(本地侧单帧 ≤128KB,分帧适配 SSH 窗口)
3=CLOSE            双向,关闭该连接
```

### 关键设计决策

- **一会话一复用通道**(`RawForwarder`,引用计数):同会话全部原始转发
  共用一条 exec 通道与 helper 进程;最后一个转发移除或会话关闭时回收
  (关 stdin → helper 收 EOF 退出 → 关 exec 通道),无残留进程。
- **部署内容寻址**:helper 以 `webssh-rawfwd-v<版本>-<sha256 前 8 位>-<架构>`
  命名部署到 `~/.webssh/bin/`(回退 `/tmp/.webssh-bin/`),部署后执行
  `helper version` 校验(架构不符会 exec 失败)。升级/换架构自动换名,
  远端永不残留旧版;同版本复用,免重复上传 ~2MB。
- **上传双通道**:优先复用现有 SFTP 通道(`.part` 临时文件 + 原子改名 +
  0700);SFTP 被禁用时回退 exec + base64 注入(数据走 session stdin,
  无命令行长度限制)。两路都失败则报明确错误,不会出现"监听成功但隧道
  不通"的假成功。
- **自动降级**:请求 `mode=auto` 时,先探测 sshd 配置(`checkForwardAllowed`),
  禁止则直接建原始模式;配置允许但运行中被拒(`*ssh.OpenChannelError`
  Prohibited)时,首次失败即原地热切换到原始模式并重试,监听端口不变、
  客户端无感,转发列表中模式标签变为「原始」,面板提示切换事件。
- **背压**:每连接下行缓冲上限 512KB,有界积压;单连接消费停滞不会拖垮
  复用通道或其他连接。单通道共享 SSH 窗口,单连接打满带宽时多连接排队
  (与 OpenSSH ControlMaster 同特性),属绕开转发禁令的固有代价。

## 后端代码结构

| 文件 | 内容 |
| --- | --- |
| `tools/socks5-helper/main.go` | 远端辅助程序(独立 module,零依赖) |
| `internal/sshmgr/rawhelper/*.gz` | 内嵌产物(linux amd64/arm64,`make helper` 产出) |
| `internal/sshmgr/rawhelper.go` | 架构探测、上传(SFTP/base64)、缓存校验 |
| `internal/sshmgr/raw_forwarder.go` | RawForwarder 复用通道、rawConn(net.Conn 适配)、帧协议 |
| `internal/sshmgr/forwards.go` | Forward 的 `Mode` 字段(std/raw)、模式裁决、auto 热切换 |
| `internal/handler/forwards.go` | API `POST /api/sessions/:id/forwards` 的 `mode` 参数 |

标准转发路径(`mode=std`)与 K8s 转发不经 RawForwarder。

## 前端

`frontend/src/components/ForwardPanel.vue`:

- 「本地端口转发」与「SOCKS5 动态代理」表单各有一个模式下拉:
  **自动**(默认)/ **标准** / **原始**;请求体携带 `mode` 字段。
- 转发列表中原始模式条目显示「原始」标签(`f.mode === 'raw'`)。
- sshd 禁止转发的告警以 warning 呈现,并提示改用自动/原始模式。

## 部署

```bash
make helper   # 交叉编译 helper(linux amd64+arm64)→ internal/sshmgr/rawhelper/
make build    # 前端 + helper + 单二进制 webssh(日常直接 make build 即可)
```

- 产物 `rawhelper/*.gz` 随源码入库,`go build`/`go test` 开箱即用;
  修改 helper 后重新 `make helper` 即可(内容寻址自动失效旧版)。
- 运行时无需任何手工部署:首次在目标主机建原始模式转发时自动上传;
  远端仅需可执行上传的二进制与 `uname`/`base64`(coreutils,SFTP 被禁时
  回退路径用),无 Python/shell 依赖。
- 安全:helper 文件权限 0700、位于用户主目录或 /tmp;不监听任何端口;
  本地监听仍强制 127.0.0.1;功能经既有鉴权体系(会话归属校验)使用。

## 测试步骤

自动化(本仓库):

```bash
make test         # 单元测试:帧协议、连接映射、32 并发无串流、
                  # 通道死亡回收、目标校验、拒绝识别、内嵌产物架构
make integration  # 端到端(自建 testssh):SFTP 真实部署 → 原始模式本地转发
                  # / SOCKS5 → 16 并发 → 停止/关会话后远端无残留进程 →
                  # deny-forward 实例(AllowTcpForwarding=no 同款拒绝)上
                  # auto 模式被拒后热切换原始模式,流量照常
```

对真实 `AllowTcpForwarding=no` 主机的手工验收:

1. `sshd_config` 设 `AllowTcpForwarding no` 并 reload。
2. webssh 连接该主机,转发面板:「自动」或「原始」启动 SOCKS5(如 1080)。
3. `curl --socks5 127.0.0.1:1080 http://<目标>` — 应返回目标内容。
4. 原始模式本地转发(监听 8080 → 远端 web:80):
   `curl http://127.0.0.1:8080` — 应返回目标内容。
5. 并发验证:`ab -n 200 -c 20 -X 127.0.0.1:1080 http://<目标>/` 或多个
   curl 并行,内容互不串扰。
6. 面板点「停止」或断开会话,远端 `pgrep -af webssh-rawfwd` 应为空,
   `~/.webssh/bin/` 下缓存文件可留存(内容寻址,复用无碍)亦可删除。
