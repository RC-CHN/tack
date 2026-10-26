# 单二进制客户侧 Node

客户机器运行一个 `tack-node`（Windows 为 `tack-node.exe`），同一进程内包含管理执行器和 Chisel 客户端库。无需再安装或启动一个 Chisel 客户端。脚本仍由目标机已有的解释器执行；可选 BusyBox 是独立配套文件，并没有打进这个二进制。

托管侧运行一个 `tack-control`，通过其 `tunnel` 配置启用内嵌 Chisel 服务端；参见[托管端安装与配置](unified-control.md)。管理 HTTPS 与隧道 HTTPS 仍使用各自的连接、监听器和凭据。Node 内两条连接独立重连；重启统一控制端会同时断开它托管的管理和隧道连接，Node 的本地任务按原超时与恢复语义继续。

## 配置与启动

先在托管 Chisel 服务端创建该节点专用、受目的地址 ACL 限制的账号，并记录服务端 SSH SHA-256 指纹。管理 token 与隧道密码不能混用。将以下对象写入控制机上的私密 `customer-tunnel.json`，替换占位值和获准的转发目标：

```json
{
  "server": "https://tunnel.example.com:8444",
  "auth": "customer-a-node-01:REPLACE_WITH_NODE_TUNNEL_PASSWORD",
  "fingerprint": "REPLACE_WITH_COMPLETE_SERVER_SHA256_FINGERPRINT",
  "remotes": ["127.0.0.1:15432:127.0.0.1:5432"],
  "keep_alive_seconds": 1800
}
```

该例将客户机本地 `127.0.0.1:15432` 转发到隧道服务端侧 `127.0.0.1:5432`；按实际授权目标调整。`remotes` 使用 Chisel 的端点语法，支持正向、反向和 SOCKS/UDP 功能，服务端权限仍然生效。Node 不接受 `stdio` 端点。私有 CA 可在对象内增加客户机上的绝对路径 `ca_file`。当前内嵌隧道入口使用原生 TLS，验证证书与 SSH 指纹，不提供跳过验证的配置。管理连接的可选 TLS profile 不会自动套用到隧道。

创建节点配置时附加 `-tunnel-config`：

```sh
tack provision \
  -config /operator/config/controller.json \
  -node-config /operator/config/customer-a-node-01.json \
  -node customer-a-node-01 \
  -server https://control.example.com:8443 \
  -state-dir /chosen/state \
  -work-dir /chosen/work \
  -tunnel-config /operator/config/customer-tunnel.json
```

CLI 将对象写入生成配置的 `tunnel` 字段；不将隧道口令写进 Controller 的管理凭据列表，也不自动创建或修改 Chisel 服务端账号。配置中的 `tunnel` 对象启用隧道功能；省略时只运行管理功能。

客户机只接收它自己的 Node 配置及最终 `tack-node`，另外按需要提供 CA/BusyBox 文件。准备配置指定的状态与临时目录后运行：

```sh
/chosen/bin/tack-node -config /chosen/config/node.json
```

Windows 使用相应 `.exe` 和 Windows 绝对路径。外部部署 skill 负责安装位置、运行账户、挂载等待、开机启动、升级与卸载，Node 不修改这些设置。

## 生命周期与检查

- 先获得 Node 状态目录的独占锁并恢复任务记录，再启动内嵌隧道；第二个实例不能借用同一状态目录启动隧道。
- 管理与隧道各自重连。隧道连接失败持续退避重试，不中断正在执行的任务；不可恢复的本地启动/服务错误使整体退出，由外部服务管理器处理。
- 正常退出取消任务、保留任务证据、关闭隧道连接与监听器并等待后台资源退出。配置在启动时加载，更新配置需要按任务排空策略重启整个 Node；集中下发隧道配置和热更新尚未实现。
- 验收需要分别执行一个明确指定节点的无害任务，以及一次授权端点的实际数据传输。节点在线、进程存在或本地端口打开，都不等于隧道可用。
- 默认关闭全部常规诊断日志，包括内嵌 Chisel；`-log-stderr` 或有界轮转 `-log-file` 同时作用于两部分。任务结果和去重状态照常持久化。

`keep_alive_seconds` 只控制隧道的 SSH keepalive：省略沿用 Chisel 的 25 秒默认值，0 关闭，可显式设置为 1800 秒等值，上限 86400 秒。它与管理心跳的默认 1800 秒 ±50% 抖动是两个配置。增加间隔前应确认实际 NAT/代理空闲超时；任务下发仍由管理长轮询即时唤醒。

## 构建与兼容性

从仓库根目录构建：

```sh
GOTOOLCHAIN=go1.26.3 CGO_ENABLED=0 go build -trimpath -o build/tack-node ./cmd/tack-node
```

发布脚本也使用此入口，先将管理执行器和 Chisel 链接进同一个文件，再整体 Garble 混淆并尝试 UPX 加壳。`BUILD.json` 的 `capabilities` 包含 `management`、`embedded-chisel`，同时记录加壳结果与例外。v0.1.2 的隧道服务端同样编入 `tack-control`，不再单独发布 `chisel`。

合并版提供 Modern 与 Linux Legacy 两种构建，均包含完整管理和隧道功能。Legacy 从同一入口通过 `go.legacy.mod` 构建，保留当前 SSH 修复；最终 amd64 混淆产物已在 CentOS 6 的 2.6.32 内核虚拟机进行功能验收。该旧内核无法启动已测试的 UPX 产物，因此 Legacy 明确保留混淆、跳过加壳。按目标系统选择一个包，选包流程与兼容边界见[兼容说明](legacy-compatibility.md)。

`management/node/cmd/tack-node` 和 `transport/browser/cmd/tack-node` 保留为管理模块开发入口，配置了 `tunnel` 会明确报错，不能用它们替代统一客户端。Windows、ARM/MIPS 的交叉编译与真机验证分别记录；Legacy 不表示支持 Windows 7。

`scripts/smoke-unified.py` 使用最终产物在本地回环地址验收两项功能、隧道断开期间的任务、恢复及退出清理。历史管理专用 Node 的内存/加壳数据不代表合并版；测量与未验证项见[验证记录](../management/VALIDATION.md)。
