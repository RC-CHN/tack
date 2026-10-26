# 单二进制托管端

托管机运行一个 `tack-control`，同一进程包含管理控制端和 Chisel 隧道服务端。客户机运行一个 `tack-node`，包含命令执行器和隧道客户端。`tack` 是管理员按需调用的 CLI，不是另一个常驻服务。正式发布提供 `tack-control`、`tack-node` 和 `tack` 三种组件。

合并的是程序和生命周期。管理接入、隧道接入、管理员 API 仍有各自的监听器；管理 token 与隧道账号也仍然分开。管理员 API 只绑定字面量回环地址，不对客户机开放。

## 新安装

先用 `tack provision` 建立控制端和节点管理配置，见 [management guide](../management/README.md)。准备托管机的配置、状态目录及 TLS 证书，路径由部署环境决定。以下 `/operator/...` 仅为示例，程序不会选择安装位置或注册开机服务。

生成一次持久化的隧道 SSH 密钥：

```sh
tack-control -tunnel-keygen /operator/config/tunnel-key
```

该文件仅留在托管机。升级和重启继续使用同一密钥，避免客户机固定的服务端指纹改变。

创建隧道账号文件 `/operator/config/tunnel-users.json`，为每个节点设置单独密码和获准的目的地址。以下仅允许示例数据库端口，替换密码及目标：

```json
{
  "customer-a-node-01:REPLACE_WITH_NODE_TUNNEL_PASSWORD": [
    "^127\\.0\\.0\\.1:5432$"
  ]
}
```

账号文件不是管理 token 列表。不要把 Controller 管理配置、隧道服务端密钥或完整账号文件交给客户机。用文件权限或 Windows ACL 限制这些文件的读取。

在刚由 `tack provision` 生成的 Controller 配置对象中增加 `tunnel` 字段，保留生成的管理员和节点条目：

```json
{
  "tunnel": {
    "listen": "0.0.0.0:8444",
    "key_file": "/operator/config/tunnel-key",
    "auth_file": "/operator/config/tunnel-users.json",
    "tls_cert": "/operator/config/fullchain.pem",
    "tls_key": "/operator/config/private-key.pem",
    "reverse": false,
    "socks5": false,
    "keep_alive_seconds": 1800
  }
}
```

这是要合并的字段示例，不是完整 Controller 配置。`reverse` 和 `socks5` 仅在实际需要并已配置相应权限时启用；转发目的地址继续由账号 ACL 控制。部署还应限制隧道到管理员端口、控制基础设施和其他客户网络的访问。同一进程内的独立监听器不提供操作系统级隔离。

`keep_alive_seconds` 省略时沿用隧道的 25 秒默认值，0 关闭，可显式设为 1800 等值，上限 86400。它独立于 Node 管理心跳，不控制命令下发延迟。

读取公开的 SSH 指纹：

```sh
tack-control -config /operator/config/controller.json -tunnel-fingerprint
```

将指纹、隧道 HTTPS 地址、该节点的隧道账号和获准转发规则补入刚生成的 Node 配置的 `tunnel` 对象，字段见[统一 Node 指南](unified-node.md)。后续添加节点时可用 `tack provision -tunnel-config FILE` 一次生成完整配置。客户机仍同时验证 HTTPS 证书和 SSH 指纹。管理连接与隧道连接可以使用不同域名、端口和 CA。

启动唯一托管服务：

```sh
tack-control \
  -config /operator/config/controller.json \
  -state /operator/state/tasks.json \
  -listen 0.0.0.0:8443 \
  -admin-listen 127.0.0.1:8081 \
  -tls-cert /operator/config/fullchain.pem \
  -tls-key /operator/config/private-key.pem
```

上面的命令行 TLS 选项用于管理接入；`tunnel` 内的 TLS 文件用于隧道接入。TLS 终止在反向代理时，后端明文监听必须绑定字面量回环地址，外部 Node 仍连接并验证代理的 HTTPS。为管理长轮询配置足够的代理读超时，为隧道启用 WebSocket 升级。

常规诊断默认关闭。需要排障时使用 `-log-stderr` 配合有界的服务管理器日志，或使用有界轮转 `-log-file`；这些选择同时作用于内嵌隧道。不要使用无限增长的输出重定向。

## 验收与常驻

按[统一 Node 指南](unified-node.md)在客户机前台启动 `tack-node`，分别验证一个明确指定节点的无害命令和一次授权端点的实际隧道传输。确认两项功能都正常后，由[部署 skill](../skills/tack-deploy/SKILL.md)按目标系统安装托管端和节点的服务定义，并记录停止、重启和卸载方式。

## 运行与升级边界

- 任一监听器启动失败应视为整项服务启动失败。退出时统一关闭管理请求、隧道监听器、已建立连接和认证文件监视器。
- Controller 重启会同时中断它托管的管理和隧道连接；Node 按各自重连策略恢复，本地任务和待上传结果保留原有恢复语义。
- 管理节点配置仍在启动时读取，`tack provision` 后需要重启 Controller。隧道账号文件的更新不等于立即断开已经认证的会话；即时撤销既有连接需按部署策略处理。
- 撤销节点要同时处理管理凭据和隧道账号。不要以删除一个管理 token 代替撤销隧道权限，也不要为恢复连接而重新提交已执行任务。
- Node 提供 Modern 和 Linux Legacy 两种构建。统一 Controller 使用现代工具链；CentOS 6 兼容范围属于 Legacy Node，不扩展到托管端。
- 后续升级前备份完整 Controller 状态及其 `.history`、`.uploads`、`.operators.json`，并保存隧道密钥、账号文件和 TLS 配置。Node 保留完整状态目录；按任务排空策略替换程序后重新验收命令和隧道。

发布构建将整个控制端与隧道库链接后统一处理。Controller 的 `BUILD.json` 应声明 `management` 和 `embedded-chisel-server`；Node 则声明 `management` 和 `embedded-chisel`。两者分别验证产物哈希、版本与适用平台。并行发布的矩阵和校验顺序见[发布流程](releases.md)。
