# Node / Chisel 整形与 TLS 指纹：集成验收

> 历史验证摘要，记录原测试时的状态。本次只提交摘要；原始 JSON/log、源码快照、校验清单及分析脚本保留在本地 `management/validation/2026-10-24-obfuscation/`，不随仓库分发。下文所述本地文件和校验命令依赖该归档，未在本次整理中重跑基准或改写原始证据。

日期：2026-10-24。Node 控制、上传 HTTPS 与 Chisel WSS 已接入双向 `bounded-v1`；现代构建可组合 `chrome-133` ClientHello 模板。最终验收包含三种模式、每种三轮，18 份 Node 报告和 9 份 Chisel 报告全部通过。**这是低占用与故障恢复验收，不是真实 GFW 通行或抗分类验收。**

默认仍为 `native/off`，先启用服务端再启用客户端。[配置与边界](../traffic-obfuscation.md)说明现代构建、Legacy、代理、mTLS 与回滚限制。实现不兼容 AnyTLS。

## 结果

下表的空闲 RSS 是三轮中位数，峰值取资源及故障测试中全部进程样本的 RSS/VmHWM 最大值，单位 MiB = 1048576 字节。只统计独立 Node 或 Chisel 客户端 PID，不含 Controller、服务端、故障代理或任务子进程。

| 指标 | 原生 `native/off` | 仅整形 `native/bounded-v1` | 指纹 + 整形 `chrome-133/bounded-v1` |
| --- | ---: | ---: | ---: |
| Node 空闲 RSS 中位数 | 10.14 MiB | 9.97 MiB | 11.35 MiB |
| Node 观测峰值 RSS | 14.61 MiB | 13.71 MiB | 15.88 MiB |
| Node 严重链路完成数 | 3/3 | 3/3 | 3/3 |
| Node 完成时间中位数 | 27.62 s | 27.87 s | 34.89 s |
| Node 三轮完成时间 | 22.85 / 27.62 / 35.15 s | 27.86 / 31.88 / 27.87 s | 31.12 / 35.89 / 34.89 s |
| Node 严重链路双向流字节中位数 | 109452 B | 115797 B | 156870 B |
| Chisel 空闲 RSS 中位数 | 11.59 MiB | 11.61 MiB | 12.99 MiB |
| Chisel 观测峰值 RSS | 12.48 MiB | 12.58 MiB | 14.06 MiB |
| Chisel 启动并完成回显中位数 | 6.13 s | 6.30 s | 6.34 s |
| Chisel 三次断流均恢复的轮数 | 3/3 | 3/3 | 3/3 |
| Chisel 恢复并回显中位数（9 次） | 6.23 s | 6.37 s | 6.39 s |

完整模式 Node 空闲 RSS 比原生增加约 1.21 MiB，Chisel 增加约 1.40 MiB；Node 严重链路中位完成时间增加约 26.3%。仅整形组稍低的 RSS 不能解释为节省内存：短样本包含运行时及测量波动。三次成功也不是统计意义上的通用完成率保证。

每种模式的 Node 有 6 个 idle/recovered-idle 窗口，Chisel 有 3 个空闲窗口；所有窗口均观测到 **0 CPU tick、0 流字节、0 storage-write byte、0 write-syscall byte 增量**。窗口约 5 秒，CPU tick 粒度有限，不能据此声称零 CPU 或零功耗。Chisel 夹具明确禁用 SSH keepalive，生产配置中启用 keepalive 仍会产生相应流量。本轮 Node/Chisel 最大退出时间分别为 2.55/2.35 ms。

严重链路流字节包含握手、业务、协议开销与断流重传，不是纯填充成本。完整模式在频繁重连下流量明显增加，不能以独立填充实验的约 1% 成本代替实际应用成本。

## 条件与口径

- Linux amd64，Intel Xeon CPU Max 9470C，104 个可用 CPU；Go 1.26.3、CGO=0、GOMAXPROCS=2、GOGC=100，普通 strip 开发构建，未叠加 Garble/UPX。版本与 SHA 见 构建记录（本地归档：`management/validation/2026-10-24-obfuscation/final/summary.json`）、二进制校验（本地归档：`management/validation/2026-10-24-obfuscation/application-binaries.json`）。
- 最终原始矩阵及 Chisel 补测均为三个模式并发 worker，每个模式内顺序执行，测量时不编译。资源来自独立 PID，但共享宿主机调度；时间结果不是专机隔离测量。第一、二次试验顺序执行，不能把跨试验耗时差全部归因于代码。
- Node 资源夹具运行 2 MiB 输出任务，覆盖 idle、upload-blocked、offline、recovered-idle，保留 32 MiB RSS 门槛。
- Node 严重链路：增加 600 ms RTT，每连接每方向 8 KiB/s，每 4 秒断流 500 ms；任务结果 32783 B（32 KiB stdout + 15 B stderr），75 秒内必须完整提交、校验内容及只执行一次。计时从首次上传探测放行开始，不含进程冷启动与任务执行。
- Chisel：同样增加 600 ms RTT、每方向 8 KiB/s；每轮先建立真实 TLS + WebSocket + SSH 转发并回显 1 KiB，观察空闲，再断流 500 ms，恢复并回显成功后才触发下一次，累计三次。恢复计时从网络恢复后开始。**不是 Node 每四秒固定断流的相同故障节奏。**
- 故障注入在用户态字节流代理上进行，没有本轮内核 netem 随机 IP 丢包数据，没有真实跨境节点或 DPI 分类器。

## 整形与指纹的独立测量

当前每方向前 8 个非空帧使用有界拆分/填充，发送的版本前缀、帧头、填充总和不超过 **256 B**；TLS 记录及 AEAD 开销另计。接收端仍接受旧 bounded-v1 的 1024 B 总上限。后续原样透传，不增加空闲包、专门的 goroutine、队列、人工延迟或握手 RTT。

内存分配实验（本地归档：`management/validation/2026-10-24-obfuscation/final/padding-allocations.log`）三轮均为 **1536 B/op、1 alloc/op**；结构体大小 1448 B。这是一个 framing 连接对象及其固定缓冲区，不是完整 TLS 连接或服务端包装层的占用。

独立 TLS 回显实验（本地归档：`management/validation/2026-10-24-obfuscation/final/padding-wire.json`）每组 20 次、32 KiB 数据，在相同 `chrome-133-h1` TLS 下仅开关生产整形层：上行 TLS 应用记录字节中位数从 **32922 B → 33259 B**，增加 **337 B（1.02%）**。本机回显中位数 141 → 189 μs，不是烂网吞吐量指标。

ClientHello 捕获（本地归档：`management/validation/2026-10-24-obfuscation/final/tls-wire.json`）记录密码套件、扩展与 ALPN：固定 `chrome-133` 模板保留 `h2,http/1.1` 和 ALPS，服务端选择 h1；旧 `chrome-133-h1` 修改为仅 h1。独立捕获中的首次 ClientHello 记录长度为 native 1479 B、h1 模板 1721 B、完整模板 1733 B；这些来自独立 TLS 夹具（该 native 对照未配置 ALPN），不能当作全部应用连接的固定长度或浏览器不可区分证明。

初期整形不是持续时间整形，也不拟合网站流量分布。Chisel 的 8 帧可能在 WebSocket/SSH 握手中耗尽，后续多路复用的新流不重新填充。固定 Chrome 133 ClientHello 也不是当前 Chrome 或完整浏览器网络栈。TCP 分段、SNI、服务端 TLS、IP、时序和长连接行为仍可观察。

## 保留的失败与最终证据来源

1. attempt-1（本地归档：`management/validation/2026-10-24-obfuscation/attempt-1/summary.json`）：原先 1 KiB 发送预算与原上传退避。完整模式第二轮 75 秒未完成，28675/32783 B；试验停止，矩阵未完成，保留 interruption/qualification 和原版本源码快照。
2. attempt-2（本地归档：`management/validation/2026-10-24-obfuscation/attempt-2/summary.json`）：仍为 1 KiB 发送预算，新增仅依据已确认上传进展的重试窗口。完整模式第三轮仍在 75 秒失败，仅 4096/32783 B。首次数据 ACK 之前不曾进入窗口，后续退避可停顿几十秒。原始总结果为 `passed:false`。
3. attempt-3（本地归档：`management/validation/2026-10-24-obfuscation/attempt-3/summary.json`）：最终 256 B 发送策略，并让验证通过的上传探测在 20 秒内为后续网络故障提供有界快速重试。HTTP/证书错误、完整畸形回复及 body limit 不借探测续开窗口；长期无响应恢复普通退避。Node 9 份资源与 9 份严重链路报告全部通过。Chisel 8/9 通过，一例启动约 0.23 秒后报 `Client cannot listen on 56016=>43861`，没有进入传输。端口位于系统临时端口范围，且原夹具探测 loopback、子进程绑定所有接口；据此修正端口预留和绑定范围，但没有把端口竞争当作已抓获的事实。原始总结果仍为 `passed:false`。
4. chisel-recheck（本地归档：`management/validation/2026-10-24-obfuscation/chisel-recheck/summary.json`）：夹具预留临时端口范围外的 loopback 端口至启动前，**应用二进制完全不变，重测全部三种模式各三轮**，9/9 通过。保存了修复后的夹具源码与 SHA。
5. final（本地归档：`management/validation/2026-10-24-obfuscation/final/summary.json`）：明确派生的验收矩阵，保留 attempt-3 全部 Node、wire、build、CLI smoke 记录，Chisel 整组使用补测的九份记录。不是将旧的失败 summary 改为通过，也不是只替换一份失败样本。`derivation` 字段列出来源与原因；源码比对（本地归档：`management/validation/2026-10-24-obfuscation/source-verification.json`）证实应用构建后仅 Chisel 测试夹具变更。最终 `qualification` 检查 27 份报告，严重链路/Chisel 同时要求 `completed:true` 与测试进程 exit 0。

## 功能与回归校验

实际 CLI smoke（本地归档：`management/validation/2026-10-24-obfuscation/final/cli-smoke.json`）使用被测二进制，验证 Controller TLS 启动、Node 凭证配置、任务提交/执行/结果收集，以及 Chisel CA + SSH 指纹 + 用户认证和 256 KiB 回显，完整模式通过。

原始回归日志（本地归档：`management/validation/2026-10-24-obfuscation/checks/test-checks.json`）及最终策略回归日志（本地归档：`management/validation/2026-10-24-obfuscation/checks-lowcost/test-checks.json`）覆盖 protocol/Node 的 Go 1.23.12 race、现代 root/control/integration/browser race、browser-tag Chisel、整形管理连接专项；六模块 vet 通过。最后的端口夹具变更另外通过 `go vet ./test/e2e` 和九份真实进程补测。

边界测试覆盖临时读超时逐字节恢复、HTTP keep-alive 短请求复用、旧预算接收兼容、非法长度/总量/版本、半帧写失败、CA/主机名拒绝、取消无响应握手、h2 协商拒绝、普通 HTTPS/WSS 兼容、native mTLS、Node 上传阻塞时的控制/取消、重启续传不重执行、管理凭证隔离及正反向隧道。

## 复现与产物

```sh
python3 scripts/benchmark-transport.py --output /tmp/tack-obfuscation-new \
  --runs 3 --jobs 3 --phase-seconds 5 --severe-seconds 75
python3 management/validation/2026-10-24-obfuscation/analyze.py /tmp/tack-obfuscation-new
(cd management/validation/2026-10-24-obfuscation && sha256sum -c SHA256SUMS)
```

输出目录必须为空。可用 `--jobs 1` 顺序运行以减少模式间调度干扰，但不能期待时间与本表完全一致。该汇总脚本拒绝把仅测试进程成功但传输未完成的记录判为通过。

本机已测开发产物位于 `dist/obfuscation-local/`，包含现代 `tack-node`、browser-tag `chisel`、`tack-control`、`tack` 与 SHA/构建清单；没有发布新版本或加入正式发布矩阵。历史 protection 与 transport 归档保持原校验值。
