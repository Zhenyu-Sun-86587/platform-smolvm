# 冻结候选 86dae32 的安装与 KVM 验收

2026-11-26 在隔离的 Linux x86_64 Worker 上构建、打包并安装了 Bundle `sha256:24e874e4bdddbb18c3bfb93e4562e720953bac6ae77626618276a83f0fd277d1`。安装目录为 `/var/tmp/deepfork-smolvm-build-cache/release-86dae32/installed`。

| 输入 | 固定值 |
| --- | --- |
| smolvm | `86dae32fee3b3e7ea76e8e77d35ea6836fff46f9` |
| libkrun | `b77e9d3a62bf3b6d6ba0245cb7b9e6c536772a97` |
| libkrunfw | `0cbb229e7b55698028e78fef9afe9c32797d0b3f` |
| 平台与验收工具 | `ab2199744fa89776b0ae30b1f547ba99ae7dc1d2` |
| Debian System 盘 | 805306368 字节，SHA-256 为 `065e82d9053c8a4a1ed6ba9030ccd816b3791f498576648a9a11010ee33b281d` |
| System Agent | `9da603e740a326ab2c13244703f7ce85be9f77598962255ef01ab7b3dabb565f` |
| System identity-helper | `424f7836ee4648a0d00399a5103daea3ff8bdf45d2508e8717d638bfcfbbc8ee` |
| 包内 KVM 记录 | `RuntimeManagedAcceptance/3`，SHA-256 为 `93b0aa8e46588517dfad383cca718bf6f44381db6ce44d8e0e753c2ff5977540` |

Guest Linux 6.18.60 应用了 `deep-x86_64.config` 和 `managed-identity-x86_64.config`。构建锁将 glibc 上限设为 2.30，Serve、Worker 和 libkrun 的实测最高符号版本均为 2.30。平台二进制和探针来自同一固定平台源码，System 盘在独立的 ext4 副本中写入该版 Agent，并核对两个 Guest 工具的字节及文件系统。新验证器也重读了既有 `91e2c72` v1 和 `d8cc5d5` v2 安装包。

## 九项包内测量

`metadata/managed-acceptance.json` 记录了九份日志、冻结源码、测试夹具、探针、平台二进制和运行时输入的摘要。基础五项为 OCI 双盘捕获和恢复、自动生命周期、System Fork、System 连续捕获和 System relay。另有四份独立日志绑定同一个测量包 `sha256:c9e504be28353bca0d0d9ac86955f0cf569d20ef7733b0e1b55bba85673a7c8e`，以及同一个 System 模板。

| 包内日志 | 实测结果 |
| --- | --- |
| `packet-native.log` | FixedInternal → Native，验证 TCP 唤醒、持久休眠后精确恢复、短 UDP 首包交付和超界 UDP 丢弃。 |
| `packet-fixed.log` | Native → FixedInternal，同样核对目标身份、地址、唤醒和 UDP 边界。 |
| `live-resources.log` | parked-capture 通过公开配置调整在线 CPU 和 balloon。platformd 在等待 balloon 时被强制结束，重启后原操作分别报告已确认的 CPU 变化和被拒的 balloon。 |
| `live-resources-hot.log` | live-compute 通过公开配置达到 2 vCPU、640 MiB、16 MiB balloon 和 1500 毫核。清除配额后恢复两个已分配 vCPU 槽的内核额度。 |

## 安装后复测

| 验收 | 实测结果 |
| --- | --- |
| 原子安装与 OCI 启动 | 安装器复核预期 Bundle 摘要。打包前和安装后各启动两个真实 OCI Guest。两份证据逐字节相同，SHA-256 为 `e0e81a03dcc853c29214c8dcfbb2482baee47559edb8c6e4a3f7b7f7d60fd0af`。 |
| 运行进程握手 | 从原安装目录启动 Serve 与 root Worker，核对 `/proc/<pid>/exe`、Worker socket 的 `SO_PEERCRED`、Worker Health 和 Bundle。实测 Serve SHA-256 为 `9b6d7542f527f15efd5adab8d635b2d92c3d64e590f32aadfdf81ddebd36c5d1`，Worker 为 `e210f26b99612981011e19c773451d34af3652d337cc164793ec6388a04dcd51`。 |
| 发布能力 | 握手公布 `boot.live_compute_growth` 的已验 profile `managed-live-compute-v1`，并确认 System 恢复、Standby 与 Wake、Native、FixedInternal、TAP 和 TAP restore 的实测 profile。单列的 `network.native_reidentity` 状态为 `unknown`。 |
| 公开资源设置 | 从同一安装件再次运行两种 System profile。parked-capture 在平台重启后恢复部分成功回执。live-compute 热增 CPU 和 RAM，核对 Guest 与宿主读回，并验证 1500 毫核和清除后的实际额度。 |

首次装后资源复跑使用的暂存父目录不允许降权的 VMM 穿越，创建中止。调整该目录的穿越权限后，两种公开场景从同一安装件复跑通过。工作区记录为 `.build/release-86dae32/`，两次装后资源结果分别见 `installed-live-resources.log` 和 `installed-live-resources-hot.log`。原始暂存错误保存在 `installed-live-resources.initial-scratch.log`。安装握手文件为 `out/installed-handshake.json`，SHA-256 为 `6a38b9fa293abe8611a512b7c8518cfdf4d0402fd110a1b4a54486738ea8359a`。

## A01 同一批次的共享 RAM 和宿主写入

从已安装的 `86dae32` 启动一个 OCI 源和两个真实 KVM 目标。批次 `observe-installed-fork` 从提交到完成只公开 `ram-f733960589c25640e72d82464cf228b3`。Worker 的记录仅有一个 volatile immutable descriptor。两个目标分别持有自己的 active reader lease，并打开了同四个 sealed backing。捕获前的源 VMM 已经打开这四个 backing，设备、inode 和大小逐个相等。

每台目标的逻辑 RAM 范围为 1,107,689,472 字节。夹具对每个 extent 用独立的只读 FD 取得 `SEEK_DATA` 和 `SEEK_HOLE`，再核对两台目标的 `/proc/<pid>/maps`。两台目标的所有数据区间共享相同设备、inode 和文件偏移，各有 125,964,288 字节使用共享 backing 的私有映射，稀疏范围按匿名零页提供。原始记录保存每个范围的 GPA、长度、backing offset 和目标 VMA。

从提交到两个目标发布后的采样，Worker、Serve、源 VMM 和两个目标的 `/proc/<pid>/io` 合计 `write_bytes` 为 3,325,952 字节，`wchar` 为 3,745,737 字节。变动文件包括 SQLite、块头、日志和两个 26,084 字节的 CPU/设备 checkpoint 文件。批次沿用源 RAM backing，发布时没有整 RAM 文件或持久 StatePoint。

用已冻结的 OCI 计数器独立复跑时，各子沿用 57,683,968 字节的 backed data，五进程的 `write_bytes` 为 4,182,016 字节。工作区日志为 `.build/release-86dae32/installed-a01-fork-backing-v2.log`，SHA-256 为 `c7ec4856cf4682de66295e35ad6abd21ee3ebf530d24de70770864be835ccbb3`。

## A02 删除祖先后的冷页、盘与 FD 偏移

同一批次的第一子机在保持根代 56 MiB RAM、两个原盘扇区和两个已打开块 FD 偏移的同时，重写最后 8 MiB 和两个新的盘扇区。夹具用 pidfd 和 Worker 空槽确认根已退出并移除其状态目录、运行目录和控制 socket，然后从第一子机再 Fork。叶子开始等待后，夹具同样确认中间祖先和兄弟已退出，并移除各自路径。离开这些路径后，叶子继续原 Guest 进程 446。

夹具把 Guest 报出的六个 GPA 逐项映射到叶子的实际文件 offset。第一次读取前，宿主 `pagemap` 的六个条目均为 non-present。叶子逐字节检查完 64 MiB 后，六个条目均为 present。两块盘读回根代与子代标志，原 FD 在 8229 和 8230 字节处继续读取，各前移 17 字节至 8246 和 8247。叶子再次 Fork 后，新的目标仍用原 Guest PID，并核对 RAM、两代盘字节和两个已前移的 FD 偏移。

复跑使用 [installed_fork_backing_kvm.py](../../tests/integration/installed_fork_backing_kvm.py)。A01 可传入冻结 `accept-block-counter`。A01 与 A02 联合复跑先以静态 C 工具链编译 [cold-fork-guest.c](../../tests/fixtures/cold-fork-guest.c)，将结果传给 `--guest` 并设置 `--lineage`。两个模式均传入 `--bundle` 安装目录和 `--smolvm` 冻结源码。联合复跑 Guest 的 SHA-256 为 `1df043ef1d6599a470f7ff9662c81dc0e67ec94de97ee2b35196a6c02b14ae21`。两段 JSON 保存在 `.build/release-86dae32/installed-a01-a02-lineage-final.log`，SHA-256 为 `637fae59ed8a2a58cc8db325b06e630bae5f9b1381b13628672964247bffa6fd`。

## A03 递归分支独立写入、休眠和末引用回收

同一安装件在真实 KVM 中从根 Fork 左右兄弟，随后由左分支 Fork 中间分支，再从中间分支 Fork 叶子。各分支每轮先核对上一轮的 1024 个私有页和两个写盘扇区，再写入由分支与 tick 决定的新值。根与左右兄弟同时运行，各推进四个 tick，并读回各自不同的标记。中间分支从 tick 13 推进到 16，叶子从 19 推进到 22，均保留 Guest PID 446。根、左和中间三个祖先分别经 Worker 围栏和 pidfd 确认退出，再移除各自的状态目录、运行目录与控制 socket。

叶子通过 Native API 单独 Hibernate。Worker 确认原执行退出且绑定空槽，右兄弟仍写页和盘并推进至 tick 143。随后叶子以 `exact` 模式从同一持久清单恢复，逻辑实例保留，执行 epoch 从 1 变成 2，Guest PID 446 从 tick 22 精确续到 23。设备库存中的三个原卷保留各自身份，其中两个数据卷在首个恢复 tick 继续验证原分支写入。

末个 volatile reader 退出前，Worker 与右兄弟共同持有四个无目录链接的 RAM backing，合计实分配 60,522,496 字节。右兄弟退出并清理后，三个 capture 的资源和传输均进入 `deleted/collected`，active root ref 和未释放 FD lease 均为零。Worker、Serve 和恢复后的叶子均无对应 backing 的 FD 或映射。回收前后的三代批次与四个子机的逻辑谱系完全相同，叶子的 durable StatePoint 仍为 `available`，恢复后的叶子继续写页和盘至 tick 26。

复跑时静态编译 [branch-lineage-guest.c](../../tests/fixtures/branch-lineage-guest.c)，再以 root 运行 [installed_fork_lineage_kvm.py](../../tests/integration/installed_fork_lineage_kvm.py)，传入 `--guest`、`--bundle` 和 `--smolvm`。原始 JSON 及输出在 `.build/release-86dae32/installed-a03-lineage-final.log`，SHA-256 为 `1117cad130ef10809ee38657d6acdc3b0833e80caf0b7e7b5330db1a7110bad6`。Guest 夹具 SHA-256 为 `ca7e458ec26dda1370ae944d76347ca5b7990551975d5a87052de15ab455a549`。

[d8cc5d5 历史安装证据](candidate-d8cc5d5.md) · [91e2c72 历史安装证据](candidate-91e2c72.md) · [逐项验收台账](acceptance-ledger.md)
