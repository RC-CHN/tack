# TLS 与早期包长整形：2026-10-24

> 历史验证摘要，记录原测试时的状态。本次只提交摘要；原始 JSON/log、源码快照、校验清单及分析脚本保留在本地 `management/validation/2026-10-24-transport/`，不随仓库分发。下文所述本地文件和校验命令依赖该归档，未在本次整理中重跑基准或改写原始证据。

**结论：保持 native 为默认。** ClientHello 实验已接入 Node 和 Chisel；包长原型只在独立 TLS 夹具中运行。本次资源测试 6/6 通过，但 browser profile 的严重烂网传输仅 2/3 在预算内完成，因此整体资格判定为不通过。没有真实 GFW 或流量分类器测试。

## 运行环境和范围

- Linux amd64，Intel Xeon CPU Max 9470C，共享主机；Go 1.26.3、CGO=0、GOMAXPROCS=2、GOGC=100。
- 同一源码，native 与 browser 的 Node 都用现代工具链，避免将 Go 1.23/1.26 的差异算作 TLS profile 的效果。Legacy Go 1.23.12 的单元测试另外通过。
- 实验二进制仅 strip，未 Garble/UPX；哈希、完整构建信息和命令见 `summary.json`。它们不是上一次保护产物，也未发布。
- 独立进程资源测试每阶段 5 秒，每 profile 三轮；任务输出 2 MiB，32 MiB RSS 门限。
- 严重烂网为每方向每连接 8 KiB/s、附加 RTT 600 ms，每 4 秒断流 500 ms；32,783 字节结果、75 秒完成预算。没有 netem/IP 丢包。
- 本地 TLS 微实验每模式 20 个新连接；原始 TLS 字节计数不含 IP/TCP/DNS、重传和 ACK。没有清空文件系统缓存。

## Node 实测

| 指标 | native | chrome-133-h1 |
| --- | ---: | ---: |
| 未压缩二进制 | 6,844,578 B（6.53 MiB） | 8,351,906 B（7.97 MiB） |
| 三轮空闲 RSS 的中位数 | 9.93 MiB | 11.43 MiB |
| 资源测试中观测到的最大 RSS | 14.25 MiB | 15.38 MiB |
| 资源门限通过 | 3/3 | 3/3 |
| 严重烂网按时完成 | 3/3 | 2/3 |
| 烂网三轮耗时 | 34.899 / 26.110 / 31.130 s | 31.376 / 32.139 / **75.044 s 未完成** |

最后一轮 browser 传输已持久化 28,672 / 32,783 字节，经历 18 次断流，随后按预算停止。它没有被删除、延长预算重测或从统计中排除。原始文件是 `severe-chrome-133-h1-3.json`。三轮样本不能确定差异由 profile 引起，重连退避和故障相位也有影响；同样不能据此宣称 browser 更快或一样可靠。

所有资源测试的 idle/recovered_idle 观察窗口中，新增 CPU ticks、传输字节、存储写入和诊断日志均为 0。CPU tick 是有分辨率的采样，这不代表长期零流量或零功耗。烂网六轮的退出均小于 3 ms，未触发重复执行与资源上限检查。

## TLS 握手与包长

| TLS 握手实验（1 KiB HTTPS 回显） | native | chrome-133-h1 |
| --- | ---: | ---: |
| 首个样本 ClientHello 记录总长度 | 1,479 B | 1,753 B |
| 握手加回显耗时中位数 | 1.672 ms | 1.834 ms |
| 上行 TLS 总字节中位数 | 2,748 B | 2,990 B |

`tls-wire.json` 保存实际线上记录解析出的 cipher suites/extensions，而不是只记录配置参数。使用固定 uTLS v1.8.2 的 Chrome 133 模板，并修改为 H1-only、删除 H2 ALPS，**不等于 stock Chrome 指纹**。

包长微实验的两种模式都使用 browser TLS，区别仅在是否加入原型 framing。每次发送 32 KiB：

| 包长微实验 | 无整形 | 早期有界整形 |
| --- | ---: | ---: |
| 上行应用阶段 TLS 字节中位数 | 32,922 B | 33,978.5 B |
| 回显耗时中位数（不含握手） | 139.5 µs | 189.5 µs |
| 每端每条连接的原型包装器分配 | — | 1,792 B / 1 次分配 |

额外线上字节约 1.03 KiB，约为无整形 TLS 字节的 3.21%。帧头与填充预算本身是每方向 1,024 字节；额外 TLS 记录的头和加密开销使线上增量可超过该值。包装器结构体为 1,624 字节，Go 分配尺寸为 1,792 字节；这不是整个 TLS 连接的内存或进程 RSS。

`padding-wire.json` 保存每条连接的 TLS 记录长度列表。长度发生变化只证明机制在工作，不能证明抗识别。这个微实验没有 Node 任务、嵌套 TLS 应用或真实跨境链路；不能把上表说成正式管理/隧道链路已经开启了填充。代码并非 AnyTLS 的线协议实现。

## 原始驱动的判定修正

本次原始 `summary.json` 中 `passed: true` 仅代表所有测试进程退出码为 0。运行时驱动漏设了 `TACK_SEVERE_REQUIRE_COMPLETE=1`；原有严重烂网夹具在该开关关闭时，允许把未完成作为观察结果，继续通过资源/语义检查。因此最后一轮 raw report 的 `passed: true` 与 `completed: false` 同时存在。

保留这些原始数据，不回填或改写。**资格结论以 `qualification.json` 为准：`passed: false`。** `analysis.json` 为从原始数据导出的比较。修复后的驱动显式要求完成，并二次校验每份报告的 `passed` 与 `completed`，缺少报告也不通过；新增回归测试重现“退出码 0 但结果未完成”和“证据缺失”。

## 验证与复测

通过的功能检查：根模块 race 测试；Legacy protocol/Node 测试；管理 integration race 测试；browser 模块 race/vet；Chisel browser 正反向转发、四次断流恢复及暂停退出；错误 CA、错误主机名、静默握手取消；padding 往返、长度限制及取消。CI 工作流语法、迁移覆盖账本、拼写和 diff 检查通过。新增 CI job 尚未在托管 runner 上执行。

历史保护证据内的部分源码曾被根模块 `go test ./...` 当作活动包编译，缺少配套 helper 而失败。新增父目录 `management/validation/go.mod` 隔离证据快照后根模块检查通过；旧证据文件未改写。

复测命令、启用方式及协议接入边界见 [实验说明](../traffic-obfuscation.md)。`source-hashes-after-review.json` 是审查结束时的源码清单，包含修正后的基准驱动；它不伪装成最初启动测试时的源码快照。原始运行命令和二进制哈希保留在 `summary.json`。

本目录 JSON/log 均来自这次运行或注明的派生分析；`SHA256SUMS` 覆盖归档文件。开发二进制位于 `dist/transport-experiment/`，不进入 Git。
