# 有界流量整形与 TLS 指纹

Node 管理 HTTPS（包括控制与上传两个连接池）和 Chisel WSS 已接入 `bounded-v1` 双向整形。现代构建提供客户端专属的 `personalized-v1` 和固定 Chrome 133 ClientHello 模板。目标是限制可观察的初期包长特征，同时保持低占用、空闲安静和断流恢复；**没有真实跨境 GFW 验收，也没有抗分类成功率结论**。

[专属模板数值报告](validation/2026-10-24-personalized.md)：32 份身份的模板复用及密钥新鲜性测试通过；Node/Chisel 三轮严重链路均完成。新模式空闲 RSS 中位数分别为 12.16/13.20 MiB；Node 三轮 74.31/31.89/35.65 s，尾部接近 75 s 门槛，不能以中位数掩盖恢复波动。

[上一轮固定模板 benchmark 与失败记录](validation/2026-10-24-obfuscation.md)：三种模式各三轮。当时的 `chrome-133 + bounded-v1` 模式 Node 空闲 RSS 中位数 11.35 MiB、峰值 15.88 MiB；严重链路完成时间中位数 34.89 s（原生 27.62 s）。Chisel 空闲 RSS 12.99 MiB、峰值 14.06 MiB，断流恢复并回显中位数 6.39 s。短时空闲窗口未观测到流量、CPU tick 或写盘增量；这些数字不代替专属模板的新测量。

## 模式与兼容性

| 客户端配置 | 效果 | 服务端要求 |
| --- | --- | --- |
| `native` + `off`（默认） | 原生 TLS、原有 HTTP/WebSocket | 原有服务端即可 |
| `native` + `bounded-v1` | 标准库 TLS + 双向初期整形；Legacy Node 可用 | 更新后的 Tack，启用整形 |
| `personalized-v1` + `bounded-v1` | 每个客户端生成并持久复用的自定义 ClientHello + 双向初期整形 | 更新后的 Tack，启用整形并选择 HTTP/1.1 |
| `chrome-133` + `bounded-v1` | 保留模板 ALPN/ALPS 的 ClientHello + 双向初期整形 | 更新后的 Tack，启用整形并选择 HTTP/1.1 |
| `chrome-133-h1` | 上一轮的修改模板，仅广告 HTTP/1.1、移除 HTTP/2 ALPS | 可用于原有 HTTPS 服务端；可与整形组合 |

`chrome-133` 使用 [uTLS v1.8.2](https://github.com/refraction-networking/utls/releases/tag/v1.8.2) 的固定模板，保留 `h2,http/1.1` ALPN 和模板中的其他扩展；由服务端选择 HTTP/1.1。遇到选择 HTTP/2 的服务端会明确失败，不会协商 HTTP/2 后发送 HTTP/1.1，也不会自动换回 native。它只是固定版本的 ClientHello 模板，**不是当前 Chrome，更不是完整浏览器网络栈**；HTTP、TLS 记录、连接复用和时序行为仍来自 Tack。

整形服务端同一 TLS 端口兼容普通 HTTPS/WSS 客户端，启用时整个监听器使用 HTTP/1.1（保留 ACME TLS 证书挑战）。客户端一旦要求整形，必须收到同版本确认；不支持时连接失败，没有降级重试。上线顺序为先更新、启用服务端，再启用客户端；回滚先关闭客户端整形，再关闭服务端。

TLS 必须终止在 Tack 进程。TCP 四层透传可用；普通 HTTP 反向代理、CDN 或外部 TLS 终止器无法直接解析整形后的数据。当前自定义 TLS profile 和整形客户端均要求直连、HTTPS/WSS 及证书校验；生效的 HTTP 代理配置会报错。普通 native/off 保持原有代理能力。

非 native profile 暂不支持 mTLS、证书验证回调或定制 TLS 版本/密码套件/曲线策略，显式拒绝这些配置；native 整形保留标准库 mTLS。Node token、任务去重与续传、Chisel SSH 主机密钥校验及用户认证仍按原协议执行。整形版本前缀不是凭证，也不是抗主动探测入口。

## 每个客户端生成并保留自己的模板

`personalized-v1` 首次构造客户端时，从系统随机源生成 32 字节身份种子，原子保存为权限 0600 的小文件；之后启动只读一次。Node 控制与上传两个连接池使用同一身份，Chisel 重连也保留同一身份。普通连接、断网、证书错误不会触发轮换，不增加定时器、后台线程或周期写盘。文件损坏或版本不支持时明确报错，不重新生成或降级。

每次新连接按种子重建相同结构，避免共享可变握手状态；无需常驻完整模板缓存。身份文件所在目录需持久保存，临时目录或容器临时层被清理后无法保留身份。

种子只决定**模板结构**：密码套件顺序、扩展顺序，以及一组可选能力是否出现，包括 TLS 1.2 AES-256-GCM 套件、OCSP/SCT、证书压缩、ALPS。支持 TLS 1.2/1.3，密码套件限于 AEAD，保留 RSA/ECDSA、X25519 与 ML-KEM 能力以及一致的 key-share/groups；ALPN 保持 `h2,http/1.1`，实际仍要求服务端选择 h1。每个握手新建可变状态，TLS ClientRandom、session ID、临时密钥及 GREASE/ECH 占位值继续重新生成。SNI 仍由实际目标决定。**不保存或重放 ClientHello 原始字节，不以身份种子代替 TLS 密码学随机源。**

配方固定为 `personalized-v1`，锁定 uTLS 1.8.2 并用 32 种能力组合的抓包摘要检查版本稳定性；依赖升级若改变模板需显式迁移配方版本。模板基于兼容的现有组件生成，但不是某个真实浏览器的指纹。固定 `chrome-133` 本身也有每次握手的扩展顺序随机化；新模式进一步使不同客户端的结构不同，并固定各自的结构。

这主要分散精确模板匹配，不能保证更隐蔽：排序归一化的指纹算法会消除排列变化，可选能力组合数量也有限；分类器仍可能识别这一类生成器。稀有而稳定的指纹还可能帮助关联同一客户端。没有把随机程度当成抗封锁成功率。[uTLS 的说明](https://github.com/refraction-networking/utls/blob/v1.8.2/README.md#randomized-fingerprint)也介绍了生成后复用的方式，本实现另行限制了变化范围。

Node 默认使用 `<state_dir>/tls-identity.json`，可用 `tls_identity_file` 指定其他绝对路径。Chisel 需显式指定 `--tls-identity-file /absolute/path/tls-identity.json`，目录由部署方预先创建。身份文件的首次原子发布要求文件系统支持硬链接，不支持时会报错。Node/Chisel 可显式共用同一文件；不要把生成后的身份文件打进镜像分发给不同客户端。若需要轮换，应停止该客户端后移走身份文件，下次启动才会生成新身份；正常升级、重启保留原文件。

## 线协议和成本边界

实现位于 `management/protocol/shaping`，只依赖 Go 1.23 标准库；现代指纹依赖隔离在 `transport/browser`。依据公开行为独立实现，**不兼容 AnyTLS**。

在通过校验的 TLS 内，每方向第一次非空写入带 4 字节版本前缀 `00 54 01 00`；它与首帧合并为同一次 TLS 写入。服务端读取 TLS 内首字节，非零按普通 HTTP 处理；零字节进入版本检查。未知版本关闭连接。服务端首个业务响应携带同版本前缀，客户端读取响应时验证，无新增握手往返。

之后每方向前 8 帧为 `data_length:u16be | padding_length:u16be | data | padding`，完成后直接透传：

- 数据块最多 1024 字节，初期拆分长度随机选择；当前发送策略按 512–1024 字节拆分长写入，短写入保持单帧，每帧填充最多 64 字节。
- 当前每方向发送的**版本前缀、帧头和填充合计最多 256 字节**。接收端仍独立执行 bounded-v1 的 1024 字节总上限、256 字节单帧填充上限，以接受上一版较重的发送策略；线协议不变。TLS 记录头、AEAD 及额外分包成本另计。
- 无填充专用 goroutine、队列、定时器、人工延时或空闲掩护包，空写入也不发包。TLS 握手仍有必要的超时管理。
- 临时读超时保留已消费的部分帧头、数据和填充状态，支持 HTTP 后台读中断与连接复用。半帧写失败关闭连接，不自行重放业务数据。
- 控制和上传仍是独立连接池；HTTP 连接复用、SSH 多路复用继续使用现有实现。重连会重新支付有界填充成本。
- 为避免连续断流把仍有进展的上传拖入几十秒等待，确认上传进展后的 20 秒内，将重试退避基数限制到 2 秒（实际抖动 1–3 秒）；原本更短的等待仍保留。验证通过的上传探测也可为随后的链路中断、超时或 DNS 错误提供同样的恢复窗口，覆盖尚未收到首个数据确认的情况。持续无响应时窗口过期并恢复普通退避；HTTP 错误、证书错误和完整但畸形的响应不会借由探测续开这个窗口。

该方案调整初期 TLS 写入的长度；TCP 可以合并或拆分这些记录，因此不能把它等同于 IP 包整形。它不做持续时间整形，不拟合某种浏览器流量分布，也不覆盖长连接里每个新建流的首包。特别是 Chisel 的 8 帧可能在 WebSocket/SSH 握手期间耗尽；后续代理流量仍可能保留长度与时序相关性。

## 启用

以下是开发构建命令，尚未加入正式发布矩阵，也没有叠加加壳/混淆打包。Legacy Node 可使用 native 整形；同时使用指纹需要现代 Node：

本机支持专属模板的已测 Linux amd64 开发二进制在 `dist/personalized-local/`，可直接使用其中的 `tack-node`、`chisel`、`tack-control` 和 `tack`；`BENCHMARK-BUILDS.json` 记录与报告一致的 SHA。`dist/obfuscation-local/` 保留上一轮固定模板构建。

```sh
(cd transport/browser && GOTOOLCHAIN=go1.26.3 CGO_ENABLED=0 go build -trimpath -o ../../dist/tack-node-browser ./cmd/tack-node)
(cd management/control && GOTOOLCHAIN=go1.26.3 CGO_ENABLED=0 go build -trimpath -o ../../dist/tack-control ./cmd/tack-control)
GOTOOLCHAIN=go1.26.3 CGO_ENABLED=0 go build -tags browser -trimpath -o dist/chisel-browser .
```

Controller 原有启动参数追加 `-traffic-shaping bounded-v1`，必须提供 `-tls-cert` 和 `-tls-key`。只作用于 Node TLS 监听器，管理员监听器保持独立。

Node 原 JSON 配置追加：

```json
{
  "tls_profile": "personalized-v1",
  "traffic_shaping": "bounded-v1"
}
```

保留原有 `server`、`node_id`、`token`、CA 和状态目录等字段。不能以关闭证书验证替代配置正确 CA。

Chisel 示例（替换域名、证书、密钥、凭证和 SSH 指纹）：

```sh
dist/chisel-browser server --port 443 --tls-cert server.pem --tls-key server.key --traffic-shaping bounded-v1 --auth USER:PASS
dist/chisel-browser client --tls-profile personalized-v1 --tls-identity-file /var/lib/chisel/tls-identity.json --traffic-shaping bounded-v1 --fingerprint SERVER_FINGERPRINT --auth USER:PASS https://your-domain.example:443 127.0.0.1:8080:127.0.0.1:8080
```

私有 CA 还需在客户端提供 `--tls-ca ca.pem`。整形不改变已有 SSH keepalive 设置；若部署需要完全安静的空闲，应自行按链路要求设置 `--keepalive 0s`。本次 Chisel 空闲测量显式关闭了 SSH keepalive。

## 复测与证据范围

```sh
python3 scripts/benchmark-transport.py --output /tmp/tack-obfuscation-new-run --runs 3 --jobs 3 --browser-profile personalized-v1
```

要求 Linux、空输出目录。`--jobs` 默认 1，表示串行；这里用 3 并行测试三种模式，每种模式内部依次测 Node 资源、Node 严重链路和 Chisel。各模式使用独立进程、监听端口与状态目录，但共享宿主调度器，因此须保留并行度信息。固定 Go 1.26.3、CGO=0、GOMAXPROCS=2、GOGC=100，交替测试普通 native、native 整形、指定 profile 加整形；`--browser-profile` 默认仍为 `chrome-133`，上述命令明确选择专属模板。保存源码哈希、二进制哈希与构建信息、命令、退出码及原始 JSON；未完成传输会判失败。脚本还会用真实 CLI 启动 Controller、Node 和 Chisel，检查组合模式的任务回传及 256 KiB 隧道回显。

- Node：独立进程 RSS/CPU/文件描述符、5 秒空闲及故障阶段，2 MiB 输出；严重链路为 600 ms 附加 RTT、每方向每连接 8 KiB/s、每 4 秒断流 500 ms，75 秒完成预算。从首次上传探测放行开始计时，不含初次启动与任务执行。
- Chisel：独立客户端进程，600 ms 附加 RTT、每方向每连接 8 KiB/s；测启动、5 秒空闲，以及 3 次 500 ms 断流后的恢复加 1 KiB 回显。每次恢复后再断流，**不是 Node 的固定 4 秒断流周期**；服务端、目标与故障代理不计入客户端 RSS。
- 线字节实验：采集实际 ClientHello 密码套件、扩展和 ALPN；独立 32 KiB TLS 回显测帧分配与 TLS 记录长度，用于隔离协议开销，不当作实际 Node/Chisel 业务吞吐。

这些是有界 TCP 故障夹具，不等同于 IP 丢包、重传测量或真实跨境识别率。原始未接入实验及其中未完成的样本保留在 [上一轮记录](validation/2026-10-24-transport.md)。

## 参考

[AnyTLS 协议](https://github.com/anytls/anytls-go/blob/main/docs/protocol.md)的初期有限填充与会话复用提供了思路；其 [FAQ](https://github.com/anytls/anytls-go/blob/main/docs/faq.md)也明确 ClientHello 不属于它的主要处理范围，默认填充不保证不可识别。本实现保留现有连接复用，没有另建一层多路复用协议。

[NaïveProxy](https://github.com/klzgrad/naiveproxy)复用 Chromium 网络栈，处理范围比单独替换 ClientHello 完整。[完全加密流量的公开测量](https://gfw.report/publications/usenixsecurity23/en/)说明随机字节本身也可能形成可分类特征。DNS、目标 IP、SNI、服务端 TLS 特征、总流量和长期时序仍可观察；本地 benchmark 不能据此证明跨境可用性。
