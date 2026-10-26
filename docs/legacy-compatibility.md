# 单二进制 Node 的旧系统兼容路径

Modern 和 Legacy 都从根目录 `cmd/tack-node` 构建，同一进程提供命令执行和内嵌 Chisel 隧道。每台客户机只选择其中一个 `tack-node`，不需要两个客户端或额外的 Chisel 进程。控制端、管理协议、节点配置和当前状态格式相同；安装路径与启动方式仍由外部部署决定。

## 选择范围

| 目标 | 默认选择与限制 |
| --- | --- |
| Linux amd64/386，内核 ≥3.2 | Modern，Go 1.26.3；CPU 基线为 amd64 v1 / 386 SSE2 |
| Linux amd64/386，2.6.32 ≤内核 <3.2 | Legacy，Go 1.23.12；amd64 已在 CentOS 6.10 的 2.6.32-754 内核虚拟机进行功能验证 |
| ARM v5/v6/v7 | ≥3.2 默认 Modern；3.1 系列选 Legacy；更老内核拒绝。v5 使用软件浮点，v6/v7 需匹配 VFP 能力 |
| Linux arm64、MIPS/MIPSel/MIPS64 | 提供两种构建，逐设备验证；不把 amd64 的旧内核结果推广到这些架构，也不自动选择低于 3.2 的内核 |
| Linux MIPS64el | 选包器以 4.8 为保守下限；四种 MIPS 包均明确标注 softfloat 和端序 |
| Windows amd64/386/arm64 | Modern；Windows 10 / Server 2016 起。没有 Win7/8 兼容包 |

这些是选包约束，不是对所有固件的认证。ARM/MIPS、NAS/overlay、厂商裁剪内核和断电恢复仍需目标机验收。静态构建关闭 CGO，不依赖目标机 glibc；仍依赖内核、CPU、可执行挂载和可用文件系统操作。裁剪内核需要 futex、epoll 等运行时能力。Go 的内核与平台边界见 [Linux 支持表](https://go.dev/wiki/Linux) 和 [最低要求](https://go.dev/wiki/MinimumRequirements)。

Legacy 默认**混淆但不加壳**。实测 UPX 5.0.2 处理的 Go 1.23 和 Go 1.26 amd64 文件，在该 2.6.32 内核中均以 139 退出；未加壳的 Legacy 文件正常。发布器在 `packing.status=exception` 中记录此原因，并保留 Garble 产物。其他 Legacy 架构在完成对应旧内核加壳验收前也使用这一例外。Modern 保持现有加壳流程。不能拿现代宿主机上的 `upx -t` 或 `-version` 成功代替旧内核运行验证。

## 操作机选包，客户机运行

在获准的目标机读取 `uname -s`、`uname -m`、`uname -r`；Windows 读取系统数字版本与实际 CPU 架构。不要要求客户机安装 Python。先在操作机校验可信发布的 `SHA256SUMS` 和提交，再运行选包器：

```sh
python3 -B scripts/select-node-artifact.py /operator/release/manifest.json \
  --os linux --arch x86_64 --kernel 2.6.32-754.el6.x86_64
```

输出包含 `_legacy.tar.gz` 文件名、归档 SHA-256、Go 版本、保护方式和构建验证级别。Modern 保持原文件名；Legacy 仅增加 `_legacy` 后缀，两者解包后的程序名都为 `tack-node`。选包器只读取 manifest，不下载、不执行、不替换程序，也不验证发布来源；必须先验证来源和完整性。缺少匹配包、架构未知或版本不满足约束时直接报错，不拿管理专用 worker 顶替。

部署顺序：

1. 记录目标系统、CPU/端序、挂载、启动机制及选中产物的哈希。准备明确的可执行、配置、状态与任务工作路径。
2. 将选中包解到暂存位置，执行 `tack-node -version`。这一步不带节点配置，不会注册或执行任务；失败时保留退出码与诊断。
3. 使用该节点私密配置，在前台验证一个指定节点的无害命令和一次授权隧道传输。私有 CA 使用显式 CA 文件，系统时钟必须正确。
4. 两项功能通过后，再安装适合设备的外部服务定义；复用同一套参数。CentOS 6 可采用其现有 SysV/supervisor，NAS/路由器按实际挂载和厂商设施选择。

若 Modern 在某台 ≥3.2 设备上存在已确认的启动兼容问题，可由部署者显式使用 `--runtime legacy` 重新选包并验收。不能因 DNS、密码、证书、ACL、磁盘或网络错误自动降级，更不能关闭 TLS/SSH 指纹验证。Node 内部没有自动下载、解包、重装或切换运行时的逻辑。

## 已部署节点切换

同一发行版本的 Modern 与 Legacy 使用相同协议和状态格式，可保留节点身份、配置及完整状态目录切换。先暂停派发并按任务策略排空运行中的命令，再停止旧进程、验证它已退出，才启动替换程序。绝不让两个实例共用状态目录，也不通过清空状态解决锁或恢复问题。

必须保留 `journal`、`replay.json`、`results/`、身份与恢复文件；未确认结果需要恢复上传。二进制回退与数据格式降级是两件事：当前存储 v4 不能交给只认识旧格式的历史版本读取。跨发行版本需遵守原有升级与备份要求，不能仅依据 `_legacy` 文件名判断可回退。

验证脚本会在上传被限制、结果积压的情况下替换 Modern→Legacy、Legacy→Modern，核对结果字节、实际执行次数、恢复与回收；这项测试针对同一代码版本的两个构建。它不能证明掉电后的任何文件系统都能恢复。

## 构建维护

默认发布矩阵增加 10 个 Linux Legacy Node 包，总计 46 个归档。`--node-runtime modern|legacy|both` 控制本地构建，默认 `both`；正式发布必须构建完整矩阵。两种 Node 都包含 `management`、`embedded-chisel` capabilities，`runtime_profile` 区分运行时。

```sh
python3 -B scripts/prepare-legacy.py
GOTOOLCHAIN=go1.23.12 CGO_ENABLED=0 go build \
  -mod=readonly -modfile=go.legacy.mod -trimpath \
  -o build/tack-node-legacy ./cmd/tack-node

python3 -B scripts/build-release.py --version 0.0.0-ci --native-only \
  --output /tmp/tack-compatible
python3 -B scripts/verify-release.py /tmp/tack-compatible \
  --node-runtime legacy --extract-native /tmp/tack-compatible-legacy
```

第一条 Go 命令是普通开发构建；交付使用安装好固定构建工具后的发布脚本。Garble 版本为 Modern v0.17.0、Legacy v0.14.2；Legacy 的 `GOFLAGS` 同时指定 alternate modfile，确保混淆器递归调用也使用同一依赖图。

`go.legacy.mod/sum` 与 `compat/legacy/modules.json` 固定兼容依赖；生成器从校验过的上游模块 ZIP 生成 `bin/legacy-deps`，不改现代 `go.mod` 或共享模块缓存。生成 manifest 记录上游版本/校验值、适配器及所有生成文件的哈希，摘要进入每个 Legacy `BUILD.json`。依赖漂移或源代码不符合预期时构建失败，需要审查适配器后再更新。

保留当前 x/crypto 的 SSH 连接释放修复，未回退旧版 SSH 库。适配只针对本项目使用的包：降低这些包的构建语言门槛，明确移除 Go 1.23 不具备的 SSH ML-KEM 与 FIPS 模式，并调整上游测试使用的新 benchmark API。原有 SSH 回归测试继续在 Go 1.23 下执行。Legacy 不提供浏览器 TLS profile；原生证书校验照常生效。这不是通用的 Go 扩展库兼容发行版。

Go 1.23 已结束官方常规维护；保留新 SSH 修复并不等于旧标准库获得了全部新安全更新。Legacy 是受约束的兼容路径，不能宣称与现代运行时有相同维护保障。未来升级依赖必须同时运行 Modern、Legacy 与真实旧内核门禁。

## 重现验收

在 Linux amd64 操作机安装 QEMU system x86、BusyBox、cpio、netcat、curl、OpenSSL 后运行：

```sh
python3 -B scripts/qualify-legacy.py \
  --artifacts /tmp/tack-compatible \
  --output /tmp/tack-legacy-qualification \
  --fixture-dir /tmp/tack-legacy-vm --download
```

该脚本验证归档并分别提取两种 Node，测试 Legacy 的隧道重连/退出、双向状态切换、HTTP/1.1 与 HTTP/2 的 32 MiB RSS 门禁及弱网恢复，记录各项报告。旧内核测试使用校验值固定的 CentOS 内核及 BusyBox initramfs，由 QEMU 系统模拟真实内核；不是 user-mode QEMU，也不是完整 CentOS 用户空间。虚拟机无客户数据、无持久盘，网络仅允许两个显式本机 TLS 转发端点。fixture 只供测试，不随客户客户端打包。

只重跑旧内核功能可使用 `scripts/smoke-legacy-kernel.py --bin-dir DIR --fixture-dir DIR --report FILE`。CI 在本机保护产物和最终 tag 归档上重复这些门禁，通过后才允许创建 draft。原始 JSON/log 按仓库惯例忽略；可读摘要见[兼容验证记录](validation/2026-10-26-legacy.md)。
