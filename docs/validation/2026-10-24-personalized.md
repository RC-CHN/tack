# 客户端专属 ClientHello：持久身份与成本验收

> 历史验证摘要，记录原测试时的状态。本次只提交摘要；原始 JSON/log、源码快照、校验清单及分析脚本保留在本地 `management/validation/2026-10-24-personalized/`，不随仓库分发。下文所述本地文件和校验命令依赖该归档，未在本次整理中重跑基准或改写原始证据。

2026-10-24，新增 `personalized-v1`，已接入 Node 控制/上传 HTTPS 与 Chisel WSS。第一次生成 32 字节随机身份种子并原子保存，后续重连、重启复用同一模板结构。默认配置仍不启用；[启用说明](../traffic-obfuscation.md)包含路径、兼容性和轮换方法。

只固定模板，不固定 TLS 随机数或密钥。种子控制密码套件及扩展顺序、部分可选能力；约束 TLS 1.2/1.3、AEAD 套件和必要的算法兼容性。没有额外空闲包、后台工作线程或周期文件写入。身份文件损坏、不可创建、版本不支持时直接失败，不轮换、不降级。

## 模板验证

browser race 日志（本地归档：`management/validation/2026-10-24-personalized/checks/browser.log`）中的抓包测试验证：

- 32 份客户端身份产生 32 种不同模板，64 次 TLS 1.3 握手成功；从文件重新载入每份身份后，密码套件、扩展、曲线、版本、签名算法与 key-share 组的结构及顺序保持一致。
- 每次握手的 ClientRandom 与实际 X25519 公钥字节不同；身份种子没有接管 TLS 密码学随机源。
- 额外 32 次 TLS 1.2 握手通过，遍历全部 32 种可选能力组合。固定抓包摘要防止配方或依赖升级悄悄改变 `personalized-v1`。
- CA/主机名错误被拒绝，取消静默握手关闭连接；服务端选择 h2 时失败，不发送 h1 请求或换指纹重试。

这里的“模板摘要”保留字段顺序，仅排除 GREASE 随机值、动态密钥等，不是 JA4 或某个网络分类器的检测结果。变化有 32 种可选能力组合及大量排列；排序归一化会消除排列差异，分类器仍可能识别生成器的共同特征。稀有且稳定的指纹也可能帮助长期关联。**没有真实 GFW 通行测试，也不宣称比常见浏览器模板更难识别。**

protocol/Node race（本地归档：`management/validation/2026-10-24-personalized/checks/test-checks.json`）使用 Go 1.23.12，验证持久文件的并发创建、0600 权限、重读不改写、不同身份及损坏文件拒绝；现代 browser 与 Chisel 使用 Go 1.26.3，涵盖真实正反向转发与断流恢复。相关模块 vet 通过。

## 独立进程 benchmark

原始汇总（本地归档：`management/validation/2026-10-24-personalized/raw/summary.json`）完整执行三种模式各三轮，27 份报告全部通过，进程 exit 0；严重链路及 Chisel 报告同时要求 `completed:true`。分析 JSON（本地归档：`management/validation/2026-10-24-personalized/raw/analysis.json`）保留每轮数值。

| 指标 | 原生 | 仅 bounded-v1 | personalized-v1 + bounded-v1 |
| --- | ---: | ---: | ---: |
| Node 空闲 RSS 中位数 | 10.09 MiB | 10.10 MiB | 12.16 MiB |
| Node 观测最大 RSS/VmHWM | 15.37 MiB | 14.25 MiB | 15.50 MiB |
| Node 严重链路完成数 | 3/3 | 3/3 | 3/3 |
| Node 完成时间中位数 | 35.14 s | 39.67 s | 35.65 s |
| Node 每轮完成时间 | 36.16 / 35.14 / 22.35 s | 39.16 / 47.95 / 39.67 s | 74.31 / 31.89 / 35.65 s |
| Chisel 空闲 RSS 中位数 | 11.91 MiB | 11.84 MiB | 13.20 MiB |
| Chisel 观测最大 RSS/VmHWM | 12.88 MiB | 12.81 MiB | 14.28 MiB |
| Chisel 三次断流均恢复的轮数 | 3/3 | 3/3 | 3/3 |
| Chisel 恢复并完成回显中位数（9 次） | 6.23 s | 6.38 s | 6.37 s |

新模式首轮 **74.31 s**，已接近 75 s 门槛，不能只看中位数而忽略尾部风险。原始报告保留 35 次上传请求及其时间，显示反复断流中的重试与续传；没有超时后换种子的行为。样本数小、恢复抖动明显，不推断吞吐提升或无性能回退。

所有 18 个 Node idle/recovered-idle 窗口及 9 个 Chisel idle 窗口均观测到 CPU tick、流字节、storage-write byte、write-syscall byte 增量为 **0**。窗口约 5 秒，Chisel 关闭 SSH keepalive；不等于长期零 CPU 或零功耗。

测试条件：Linux amd64 / Xeon CPU Max 9470C，Go 1.26.3，CGO=0，GOMAXPROCS=2，GOGC=100。三个模式 worker 并发，每个模式内顺序执行；race 测试在 benchmark 前完成，进程测量阶段不编译。共享宿主调度器，非专机隔离实验。

- 资源：实际外部 Node/Chisel 客户端 PID，32 MiB 门槛；Node 包含 2 MiB 输出与空闲、阻塞上传、离线、恢复空闲阶段。RSS 不包含服务端、故障代理和任务子进程。
- Node 故障：增加 600 ms RTT，每连接每方向 8 KiB/s，每 4 s 断流 500 ms，32783 B 结果须在 75 s 内完整提交且不重复执行。计时从首次上传探测放行开始，不包含启动和任务执行。
- Chisel 故障：同样延迟、带宽，三次 500 ms 断流；每次网络恢复并回显 1 KiB 后才触发下一次，恢复计时从恢复网络开始。与 Node 的固定四秒故障周期不同。
- 用户态字节流代理，没有本轮 netem 随机 IP 丢包或真实跨境检测。三轮使用独立客户端状态目录与新身份，不是某一个种子的长期稳定性试验。

## 建立连接时的成本

模板重建微基准（本地归档：`management/validation/2026-10-24-personalized/raw/template-allocations.log`）三次为 22.24–23.05 μs/op，中位 22.76 μs/op，**9080 B/op、60 次分配**。这是一个代表性种子的独立模板构造成本，每个新 TLS 连接发生一次，不是完整握手成本，也不能换算为新增常驻 RSS。实际连接会使用其中的状态，部分对象可保留至连接结束。

同批 TLS 回显实验（本地归档：`management/validation/2026-10-24-personalized/raw/tls-wire.json`）各 20 次，本机握手加 1 KiB 回显中位数：固定 `chrome-133` 为 1873 μs，`personalized-v1` 为 1886 μs。微秒差值受本机调度影响，不代表严重链路表现。

实际客户端只保留身份种子，并为每条 TLS 连接创建新可变握手状态。现代 Node 二进制 8409250 B，Chisel 12521634 B。这里与原生模式的 RSS 差异包括整个 uTLS 实现，不能全部归因于种子或模板重建；本批没有固定 Chrome 模式的独立进程资源对照。上一轮固定模板数据另见[历史报告](2026-10-24-obfuscation.md)，不能将跨轮波动全部视为代码差异。

## 证据、复现与产物

实际 CLI smoke（本地归档：`management/validation/2026-10-24-personalized/raw/cli-smoke.json`）使用同批二进制，验证专属模板模式下 Controller/Node 的凭证、任务提交/执行/回传，以及 Chisel 的 CA、SSH 指纹、用户认证和 256 KiB 回显。

源码比对（本地归档：`management/validation/2026-10-24-personalized/source-verification.json`）确认 benchmark 构建后应用、测试和脚本源码没有变化。27 份报告的二进制 SHA 均与构建记录匹配。原始结果没有替换失败样本、放宽完成门槛或重新标注；此前三个归档目录保持原 SHA。

```sh
python3 scripts/benchmark-transport.py --output /tmp/tack-personalized-new \
  --runs 3 --jobs 3 --phase-seconds 5 --severe-seconds 75 \
  --browser-profile personalized-v1
python3 management/validation/2026-10-24-personalized/analyze.py /tmp/tack-personalized-new
(cd management/validation/2026-10-24-personalized && sha256sum -c SHA256SUMS)
```

输出目录必须为空。`dist/personalized-local/` 包含已测的 `tack-node`、`chisel`、`tack-control`、`tack` 与 SHA/构建清单。这些是未发布、未叠加 Garble/UPX 的 Linux amd64 开发产物；原 `dist/obfuscation-local/` 保留上一轮版本，不支持新 profile。
