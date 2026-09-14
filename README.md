# webssh

Web 前端 SSH 管理工具(Go + Vue3 单二进制部署)。

## 功能

- **主机管理**:保存连接(名称 / IP / 端口 / 用户名 / 认证类型 / 密码或私钥),支持密码与私钥(读取本地 `~/.ssh` 密钥列表)两种认证,可选经跳板机连接。主机信息保存在 XDG 配置目录(`$XDG_CONFIG_HOME/webssh/hosts.json`,默认 `~/.config/webssh/hosts.json`,权限 0600)。
- **终端**:多会话 Tab,基于 xterm.js 的完整终端(原始 PTY 字节流透传,支持完整转义协议 / 256 色 / 滚动回看),窗口尺寸自适应。主机可勾选 **Follow SSH path**:勾选后注入 OSC 7 提示符钩子(bash/zsh 无感注入,其他 shell 回退 type-ahead),SFTP 面板据此单向跟随终端 cd;该模式经 exec 启动 shell,不显示 Last login/MOTD 横幅。默认关闭 = 普通连接,无任何注入。
- **SFTP**:上下双文件树(上=远程主机,下=本机,下方也可切换为另一台已连接主机),显示名称 / 大小 / 所属用户 / 分组 / 权限;双击目录进入,当前目录文本框回车直达;支持选中文件的 上传 / 下载 / 删除(目录递归);目录不存在等错误有醒目提示。
- **统一传输**:本机↔远程、远程↔远程 全部走同一异步任务模型(`POST /api/transfers`):目录先枚举计划再由并发 worker(单任务 4,全局上限 8)拷贝,1MB 缓冲;进度经 `/api/transfers/ws` 推送,支持取消。任务仅存内存,进程重启即丢,无断点续传。
- **主机详情**:静态信息采集与实时指标(5s),Docker 容器列表 / 详情 / 日志流 / 容器内 shell;容器 ID 统一白名单清洗 + shell 转义。
- **代理 / 转发**:本地端口转发(`-L`)、SOCKS5 动态代理(`-D`),均只监听 127.0.0.1;跳板机在连接配置中指定。

## 快速开始

```bash
cd webssh
make run          # 构建前端+后端,启动 http://127.0.0.1:8090
```

开发模式(前后端分离,前端热更新):

```bash
make dev          # 等价于同时启动 dev-backend(127.0.0.1:8090) 与 dev-frontend(:5174)
```

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
| `WEBSSH_SSH_KEYS_DIR` | `~/.ssh` | 本地私钥目录 |

## 安全说明

- **默认仅监听 127.0.0.1**:全部 API 无鉴权,服务必须只暴露给本机。如确需外部访问,请经反向代理(TLS 终结 + 鉴权)转发,不要直接改 `WEBSSH_LISTEN_ADDR` 为 `0.0.0.0`。
- **WebSocket 同源校验**:所有 WS 端点校验 Origin 与 Host 一致(非浏览器客户端无 Origin 放行),防御恶意网页对 127.0.0.1 服务的跨站 WebSocket 劫持。
- `hosts.json` 中密码为明文保存(便于免密直连),文件与目录权限收紧为 0600 / 0700,请勿在多用户机器上使用。
- SSH 主机密钥采用 TOFU(首次信任)策略,指纹记录在 `known_hosts.json`;指纹变化时连接会失败,确认安全后重新连接即可。
- 远程 shell 工作目录跟随(SFTP 面板)仅在主机勾选 Follow SSH path 时启用,依赖注入的 OSC 7 提示符钩子(bash/zsh 无感注入,其他 shell 静默跳过);fish 等 shell 下 SFTP 面板不会自动跟随终端目录,属预期行为。
