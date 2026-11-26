# WP-24 发布验收台账

2026-11-26 以 `fb69434` 和 `c011476` 冻结组合续验 [A01–A27](ACCEPTANCE_AND_OPERATIONS.md#2-核心能力完成矩阵) 与 [A28–A36](ACCEPTANCE_AND_OPERATIONS.md#122-新增必要验收)。每行记录观测到的结果和余下的组合验收。测试在隔离的 Linux x86_64 Worker 或本地测试环境进行。

## 本轮执行范围

2026-11-26 起，按项目所有者要求跳过涉及冒用身份、凭据重放、未经授权访问、规避访问控制或漏洞利用的测试。下列子项单列为「按要求跳过」，在本轮验收和进度统计中不计通过。此前的实测记录保留原日期和版本。

| 验收项 | 按要求跳过的子项 | 继续执行的子项 |
| --- | --- | --- |
| A13 | 冒用其他 attachment、尝试绕行网络访问控制 | 自有实例的双向互通、地址重叠、hairpin 与 MTU |
| A14 | 向身份门重放旧 ACK 或提交伪造的实例身份 | 正常身份交接、目标进程按阶段退出、父机继续运行 |
| A16 | 携带复制的父机凭据访问 public API 或 relay、重放已撤销凭据 | 显式授予与撤销、当前授权状态、正常 relay 请求 |
| A20 | 用过期的 TAP 租约修订或在恢复围栏期间发起业务连接 | 核对当前租约、DNS、路由回执和获准连接 |
| A21、A22 | 伪造来源身份、使用他人或已失效凭据尝试连接 | 同一所有者已授权连接的并发唤醒、服务修订、在途连接撤销 |
| A24 | 在恢复后的公开接口提交旧控制代次的写请求 | 用新会话核验新旧发行件恢复后的控制代次、同一所有者和备份 |
| A29 | 复用已消费的终端票据、以只读身份写入终端 | 有效会话的断开、持有、凭新票据重连、按序输入、退出和回收 |
| A32 | 子机使用复制的父租约令牌、冒用旧 execution 续租 | 同一所有者的续租、到期、主动释放与 UI 状态 |

若冻结夹具混合这两类操作，单独记录实际执行的子项及所用夹具版本。完整发行资格仍以该冻结版本原有的验收规则为准。

## 测试版本

| 证据名称 | 构建与运行范围 |
| --- | --- |
| fb69434 冻结安装 | 九组 KVM 日志随包发布，安装前后双 OCI 证据一致，原目录 Serve 和 root Worker 握手确认网络、System 恢复、待机与 live-compute。公开批次取消后的失败子重试与 Task 认领、手动 Stop、停机卷与首桌面通过。保存唤醒故障正确拒绝冷启，围栏后显式 exact 重试触发盘代际冲突。[安装输入、实测结果和失败边界](candidate-fb69434.md) |
| 86dae32 冻结安装 | 九组 KVM 测量随包发布。安装前后各两次 OCI 启动，证据逐字节相同。实际从安装目录启动的 Worker 和 Serve 握手公布实测的 live-compute、网络与待机能力。安装件重复通过 parked-capture 和 live-compute 的公开资源调整。[安装输入、绑定摘要和实测范围](candidate-86dae32.md) |
| d8cc5d5 冻结安装 | 七组 KVM 测量随包发布。安装后验证双 OCI 启动、Worker 与 Serve 能力握手、公共 Guest 连接崩溃恢复、双模式私网休眠切点和 Worker 压力与唤醒恢复。[安装输入、日志摘要和能力](candidate-d8cc5d5.md) |
| 冻结安装 | 已安装的 smolvm `91e2c72`，Bundle `sha256:512366a367c3f424d20c787b2144d81b1719128468212b69ef4db078e1821ffc`。安装路径为 `/var/tmp/deepfork-smolvm-build-cache/release-91e2c72/installed`。安装后用分发 wrapper 启动了两个 KVM OCI Guest，重测的 OCI 证据与打包前逐字节相同。另从安装目录启动了 Serve 并核对 reader 和 Bundle 摘要。握手使用无 Worker 配置，Worker 依赖的能力返回 `WORKER_NOT_CONFIGURED`。本地记录为 `.build/release-91e2c72/install.log`、`.build/release-91e2c72/installed-smoke.log` 和同安装目录旁的 `installed-handshake.json`。 |
| 冻结测量 | 同一冻结构建锁和产物打包前的五组真机测量。下文的 `managed/*.log` 位于该安装目录的 `metadata` 下，五份日志的 SHA-256 均与 `managed-acceptance.json` 相符。测量元数据记录平台验收代码 `b2b387a` 和 smolvm 验收代码 `9eba0db`，与 Bundle 的源码修订分别登记。生命周期实验通过 Worker IPC 注入受控制的活动报告，A29 的开发真机另使用实际收集器。 |
| 开发实机 | `.build/` 下以工作区为根的具名 KVM 记录。各实验使用当时的开发 Bundle 或 stock runtime。 |
| 历史与本地测试 | 旧冻结候选单独标明版本。Go、Rust、Python 的契约或注入测试保留其覆盖范围。 |

## 捕获、存储、网络与 Guest

| ID | 范围 | 已核实结果 | 待验 |
| --- | --- | --- | --- |
| A01 | 86dae32 冻结安装，真实双子批次通过 | 两个子机在同一个 capture ID 下取得各自的活 reader lease。源在捕获前已持有四个共同 backing。共享范围的 inode 和文件偏移通过两子 FD、内核 VMA 与 Worker 逐项核对。每子逻辑 1,107,689,472 字节，其中 125,964,288 字节使用共享 backing 的私有映射。五个相关进程从提交到发布后采样共写入 3,325,952 字节，批次未发布持久 StatePoint。[同批共享与写入记录](candidate-86dae32.md#a01-同一批次的共享-ram-和宿主写入) | — |
| A02 | 86dae32 冻结安装，同批三代冷读通过 | 源和中间祖先经 Worker 围栏退出，专属目录和控制 socket 均已移除。叶子首次读取前，六个实测映射页均未驻留于目标。完整校验 64 MiB 的根代和差量后六页均已驻留。两盘根代和中间代数据一致，两个原打开的块 FD 按 8229→8246 和 8230→8247 前移。叶子继续 Fork，下一执行验证原进程、数据和两个新 FD 偏移。[同批冷读和祖先移除记录](candidate-86dae32.md#a02-删除祖先后的冷页盘与-fd-偏移) | — |
| A03 | 86dae32 冻结安装，分支隔离、精确恢复及末引用回收通过 | 根与左右兄弟并行读写 1024 个私有页和两个盘扇区，三代递归树逐个围栏根、左和中间祖先并清理运行路径。叶子独立 Hibernate/Restore 时右兄弟持续写入。恢复后的叶子保留 Guest PID 和三卷身份，从原保存点精确推进一个 tick。最后一支退出后，三份 volatile 资源和传输均回收，旧 backing 无 FD 或映射，逻辑谱系与 durable StatePoint 仍可查。[同树 KVM 验收](candidate-86dae32.md#a03-递归分支独立写入休眠和末引用回收) | — |
| A04 | fb69434 安装件，三种 provider 场景通过 | 活源和活子分别给重启的 broker 交还原 sealed RAM。仅 broker 持有，以及 broker 与唯一源 VMM 同时退出时，恢复均报告 `lost`，保留 capture 引用，拒绝 GC，新 reader 的不可用回执携带零个 FD。开发平台另验证原 child 提供两块可写头后公开 Fork 继续原 PID。[资源身份和日志](#a04-broker-与最后一个-ram-provider)、`.build/wp23/tracked-provider-kvm.log` | — |
| A05 | 开发实机、本地测试 | 同一 Debian 块代际同时受 checkpoint、休眠根、未决操作和备份导出保护。公开删除 checkpoint 后，导出仍持有原 inode，休眠仍可恢复原 RAM 和根盘数据。单独持有操作或块导出都阻止回收。最后引用释放后 Worker 在 unlink 前被终止，恢复围栏解除后重放原 tombstone，删掉磁盘文件并清除依赖边。 [实测与复现](managed-recovery.md#statepoint-引用与实物回收) | 用下一次冻结安装复跑。 |
| A06 | 冻结调用方测试、开发 Worker 恢复，18 组实机切点通过 | 真实请求进程中途退出后仍可重取相同 checkpoint 与密封 RAM FD。真实 Worker 或源 VMM 分别在 9 个阶段退出，覆盖双可写盘切换、RAM 导出与子机启动。源停止的 6 个前期切点无子机，3 个后期切点各完成一个子机。9 次 Worker 替换均继续原决策，各完成一个子机。[请求、Worker 与源虚机的退出切点](#a06-捕获调用方退出与部分导出) | 对包含恢复修复的最终安装复测。 |
| A07 | fb69434 冻结安装，无后代与活后代两组通过 | 两组从捕获到旧 VMM 退出均保持控制台计数和四个盘头摘要。真实 chunk 路径错误后可在 STOP 前取消并回原 Standby，STOP 后 Worker 重启只收敛退出。持久根独立恢复原 PID 和卷，临时导出 pin 释放。无后代组释放 180,129,792 字节实分配 RAM；活后代组释放 30,875,648 字节并保留 163,090,432 字节共享 RAM，后代校验 96 MiB 冷数据。相同安装件另通过真实私网 SYN 停止切点。[切点、回收口径与原始记录](hibernate-cut-qualification.md) | — |
| A08 | `fb69434` 冻结对照，修复后开发实机通过 | Raw 拒绝严格独立 Fork。Raw 和 managed Standby 均保持原 VMM 和资源图，managed 保留原 Gate。冻结件将暂停计入 Guest 时间，Fork 子机和 Hibernate 恢复时已触发定时器。修复后原机 Wake 原键重放、独立子机及新 epoch 恢复均按原 Guest 9 秒到期。独立虚拟 Worker 断电换 boot 后，另一个原 Guest 定时器在 18.069 秒到期。[跨 boot 时钟](managed-recovery.md#worker-主机换-boot-后的恢复)。[实机时钟与回执](#a08-长待机和保存点时钟) | 在包含修复的冻结安装上复跑。 |
| A09 | fb69434 冻结安装，交错多盘切点通过 | 八次独立 Fork 覆盖已提交、准备、两种写入中和两种合法先盘。目标在原 PID 的 RAM 相位读回双盘计数，完整 WriterPivot 同时包含两盘。持久 exact Restore 保留未完成事务的两盘差异，再提交原事务。两个实际 capture 各取一盘的组合在两个源 RAM 绑定下均拒绝，目标无控制 socket 与 Guest 输出。[多盘状态、左右盘实测和反例](multidisk-phase-qualification.md) | — |
| A10 | fb69434 冻结 Worker 的 FD 矩阵及双开发 Bundle KVM 通过 | 同 Bundle 与跨 Bundle 的同代 RW/RW、RW/RO 争用均被拒。硬链接按同一 inode 互斥，路径符号链接拒绝登记。重启后只接受原带锁 OFD，最后一个原 OFD 关闭后才发放后继 writer。封存后两路在独立只读租约下共享同一 inode。开发实机另确认两份 Runtime Bundle 的 KVM 共用 Worker，第二路无法挂接活写卷。[矩阵与证据摘要](block-ownership-qualification.md) · [卷故障实测](#卷恢复与停机存储实测) | — |
| A11 | fb69434 原安装与 f2df146 测量组合通过 | 原持久卷在保存后推进并转挂，源删除回收当前私有写头。公开 exact 报 VOLUME_GENERATION_MISMATCH 与 effect=none，clone_conflicts 从确切捕获代恢复旧 RAM 与双盘，发布独立卷及共享只读代。平台重启、Hibernate 和 exact Wake 后均保留公开映射与数据。[安装和测量数据](volume-lifecycle-qualification.md#a11-旧保存点与独立推进的持久卷) | f2df146 System 安装授权尚未发布。 |
| A12 | fb69434 原安装通过五盘角色、停机卷与存储切点。schema 29 开发组合通过 machine 生命周期与只读共享 | root、storage、overlay、cache、data 同点保存及恢复。公开 Stop 后增长，GuestFS 阶段失败时仍保留已曝光块容量。真实 ENOSPC 保留原 head，封存 fsync EIO 在 Worker 重启后沿用暂存 inode 完成。在线拒绝保留原码并给 REQUIRES_STOPPED。machine 写头随退役回收，两个同 owner 的只读消费者保留固定世代，旧 ephemeral 保持原生命周期。独立 Fork 发布 machine 和 persistent 子卷并共享固定 RO，子机两次冷启沿用原根和两块写盘。[实测与复现](volume-lifecycle-qualification.md) | schema 29 的发行安装复测。在线块设备变化由独立 profile 声明。 |
| A13 | 自有实例的开发 KVM 私网矩阵通过 | 同域两个 FixedInternal 都使用 `10.0.2.15`，通过不同 VIP 双向通信。Native 与 FixedInternal 混合组分别保留正确源地址。自身 DNS/VIP hairpin、重叠 CIDR 两独立域的本域路由、1500 MTU 的双向 1472 字节 IPv4 DF UDP 都实测，1473 字节返回 `EMSGSIZE`。显式授权的两个独立域双向 TCP 看到原 VIP 和 Native IP，Worker 重启后原执行重建路由，撤销后两侧都移除路由。既有恢复组另外核对目标网段冲突、route domain IPAM 与旧 attachment 围栏。[四组自有实例记录](private-network-a13.md)、`.build/platform-networked-backup-native-v3.log` | 冒用 attachment、规避访问控制和 IPv6 旁路按要求跳过，且不计通过。 |
| A14 | 冻结测量、开发实机，部分 | 冻结 OCI 目标在激活前被终止，成功兄弟与 capture 保留。开发 TAP 在 park 和身份应用期间对 ICMP 沉默，失败身份保持隔离，父子保持独立；目标探针另检查早到 commit、错误 context 和后来一次 standby 上的旧 BEGIN。 `managed/oci-capture.log`、`.build/network-gate-libs/hostroute-kvm.log`、`smolvm/libkrun/tests/guest_gate/target_probe.py` | 各身份阶段断连，并核对普通进程、容器、loopback 与外部 I/O 计数和失败 child 的发布状态。身份 ACK 重放按要求跳过。 |
| A15 | 冻结安装、生产内核与 helper | `fb69434` 两轮实测 ext4 写进程处于 D 态时，三个计数器随 gate 冻结，取消后继续。未应答的 FUSE LOOKUP 在 465–479 ms 返回 `failed_thawed/EBUSY`，取消竞争也有终态和解冻证据。生产 helper 退出与普通 Agent 停止分别通过。[实测与日志](#a15-阻塞-io-取消与实际解冻) | — |
| A16 | 冻结运行时的所有者授权、撤销和同机回滚通过 | 原所有者签发 grant，在同机快照后将其撤销到 revision 2。回滚恢复原 RAM 标记，epoch 从 1 升至 2，原授权记录在回滚和平台重启后都保有同一撤销时间与版本。恢复后的执行凭新签发的授权完成一次 relay 命令，新授权的使用计数为 1。夹具 `managed_system_vsock.py --case grant-rollback`，记录 `.build/wp23/a16-fb69434-owner-grant-rollback-v1.log`（SHA-256 `b5fc7d596fb5e5ba3e47fa4b1f733b78d937069ba626bf50ed1014596770bd1f`）。 | 复制或重放凭据的访问按要求跳过。 |
| A17 | `fb69434` 冻结 System 实机与 OCI 对照，修复后 OCI 开发实机通过 | 真实 PID 1 为 systemd。Docker Compose 新构建镜像并运行容器，System Agent 重启后仍读取原进程。K3s 新节点和 CoreDNS Ready，真实 Pod 从两条 Service DNS 记录找到原 HTTP 标记。冻结 OCI Agent 退出会结束 VMM。修复后管理 Agent 的 SIGTERM 和 SIGKILL 均保持原任务、日志和显式/自动停机合同，子机保留 Guest tmpfs 工作负载登记。[版本、PID、DNS、回执与复现](guest-runtime-a17.md) | 用包含 OCI 修复的冻结安装复跑管理服务重启。 |
| A18 | fb69434 冻结安装，批次取消、父删除后重试和任务认领通过 | 公开批次保留原 capture 和已就绪 child。取消目标继续围栏，父机 Stop、Delete 和平台重启后，仅原失败 child 以原 ID 成功重试。公开 Task 认领返回同一 run、原 Guest PID 与原标记。首次公开提交的 HTTP 回执整体丢弃后，重启平台并以原键、原修订重试，只保留一个操作、一个 capture 和一个原子机；该子机读回原 RAM 标记。再次重放返回相同操作，改变请求返回 409。同键取消、重试和删除重放稳定。另有开发实机记录父私有写盘回收、子继续读共享闭包及父持久卷转挂后读回原字节。[首次回执丢失](candidate-fb69434.md#首次-fork-回执丢失) · [同包任务复测](candidate-fb69434.md#安装后公开操作) · [持久任务实测](managed-recovery.md#完整分叉与独立恢复后的任务索引) | — |

### A04 broker 与最后一个 RAM provider

探针从冻结安装 `fb69434` 加载 Worker 和 libkrun，走 WorkerResource 的真实 Unix socket 与 FD 传输。探针登记其加载 libkrun 的测试进程作为 VMM。活源重交三个原 backing 后退出，子机从 tick 4 继续，到 tick 9 读到原冷页的 `0xa7`。再重启 broker，存活子机重交三个 backing，孙机拿到六个 backing，从 tick 11 继续，三代 Guest PID 均为 419。

两个丢失实验各在独立 PID 命名空间运行，保留逻辑 capture 并移除落盘的 capture 目录。第一组围栏退出原源机，只有 broker 持有三个已封存 inode，再终止 broker。第二组确认唯一源 VMM 收到 SIGSTOP 后终止 broker 和源机。重启期间回收请求均得到 `E_RECOVERING`。恢复后两个 catalog 均保留原描述符摘要和 capture 引用，状态为 `lost`。GC 得到 `E_RESOURCE_PINNED`，新执行的只读租约得到 `E_RESOURCE_UNAVAILABLE`，内核返回零个 FD。夹具为 smolvm 提交 `69c4564` 的 `crates/smolvm-resourced/tests/kvm-generation.py`。

| `.build/wp23/` 实机记录 | SHA-256 |
| --- | --- |
| `a04-live-provider-fb69434-v1.log` | `b633f5fc91424918a941648f39010b96ba8963dbfdf43bb4145d3c7d538f3ff6` |
| `a04-broker-only-fb69434-v2.log` | `5da3b69f5f77dce711fccb6a54364b4dc1e36f40ab06c666c582b064e47849b2` |
| `a04-last-provider-fb69434-v2.log` | `9b3a1805864f5aeec95ec241d80da16b472c14f9c999cb30f4ca78a1c3913ec2` |

### A06 捕获调用方退出与部分导出

在运行中的 2 vCPU Guest 上，夹具在确认首字节已抵达 socket、调用方尚未处理应答时终止该请求进程。另四个请求进程分别在收到完整导出头、checkpoint、描述符和首个实际 RAM FD 后被终止。raw 模式分别测试继续和取消。同一 token 重新读取时，原 checkpoint、描述符、全部 backing inode 及其大小相同。三个并发的源状态修改均被拒绝。继续后原三路进程计数和盘写入恢复，取消后同一导出被拒绝。

在两块可写盘的 managed 源和递归子机上，夹具又分别在 Guest 准备、park、进入 Standby、准备 capture、两盘 writer 切换及返回原 Standby 的应答处终止请求进程。原盘切换在 Worker 中只提交一次。相同的四个导出切点分别返回原来的 4 和 7 个密封 backing。两个 held 源的 Guest 计数保持，祖先退出后孙机从原 RAM 和两盘成功继续。

失败注入只影响本次测试 VMM 打开一个已密封 RAM FD 的动作。EMFILE 后查询和相同 Prepare 均返回原 `FAILED_HELD`，导出及继续请求被拒绝，三路 Guest 计数和盘计数保持原值。夹具位于 smolvm `1452c64` 的 `tests/capture_transport_kvm.py` 与 `crates/smolvm-resourced/tests/kvm-parked-block.py --interrupt-capture`。原始记录分别为 `.build/wp23/a06-fb69434-capture-disconnect-v3.log`（SHA-256 `c6332e45b6c72163937a0d8503308eb43c27d4b459f1342e12dcd61dd5ef2928`）和 `.build/wp23/a06-fb69434-managed-disconnect-v1.log`（SHA-256 `5f8c9b9a9e63ec11c20a226b854c088622cbd9e25d1d94086e90a22088ac1759`）。

真实 Worker 和源 VMM 分别在 `arming`、`parking`、`standby`、`holding`、`pivoting`、`exporting`、`source_return`、`waking`、`children` 九个持久化阶段经 pidfd 终止。源退出的前六组终止原操作并释放 writer，没有发布子机。后三组各恢复一个子机。九次冻结 Worker `0c6454b969bd304fb98668ff963b9dc4a2b3703acd4228ea9762b8cd326f7d9c` 退出后，开发 Worker `d81a5b2f5ad8d322bacc641a1a330b54db77085bd691d2bf317e0cac91c515cd` 接管同一源 VMM，核对两盘 writer、原操作和执行身份，分别完成一个子机。子机串口输出含自己的主机名和原任务 PID，计数继续增加。另一次独立试验在恢复期间留下另一项未完操作，确认 Worker 等待该项完成处理再开放。smolvm `4651074` 提供恢复逻辑和 `tests/managed_capture_crash_kvm.py`。逐项日志位于 `.build/wp23/a06-cut-matrix-v8.sha256`，清单 SHA-256 为 `e9dc957d0ddb75ada8a527f962cab1ee19646461334b39eca3de80e8b5285951`。

### A08 长待机和保存点时钟

在同一物理 Linux KVM 宿主机上使用 `fb69434` 安装 `sha256:a9b41917173a0eea2306aa41276b59fa314e1d54f9d52f45766bcf2ce7698c35`、平台 `c011476` 和相同 Debian System 镜像。Guest 记录 `CLOCK_MONOTONIC`、`CLOCK_BOOTTIME`、`CLOCK_REALTIME`、原定时器进程和起点。Raw 等待 12 秒，两个 managed 场景各等待 24 秒。Worker 的资源、写盘 head、StatePoint 行和对象文件列表在每段 Standby 前后逐项相同。Raw 独立 Fork 收到 `E_STATE_CONFLICT`。两种原机 Wake 保留 PID、incarnation 和 epoch。同键请求重放均返回原操作。[KVM 夹具](../../tests/integration/standby_clock_kvm.py)

冻结件中 Fork 子机和 Hibernate 恢复的首次 Guest 时钟采样已经分别推进约 31.936 和 32.762 秒。修复版在只读私有挂载上替换 libkrun `0d02553` 和 Worker `f4fa5f5`，开发 Bundle 摘要为 `sha256:460a09b6b40081ed77aa3268d1fb6e0e496cbba128bf4fd3d6e9863e9599896f`。独立子机具有新身份，原机仍停在同一 hold。Hibernate 保留从 `STANDBY` 取得的 StatePoint，原 VMM 经 pidfd 确认退出。保存点额外离线 12 秒后，新 incarnation 以原 instance ID 和下一 epoch 继续。首次采样时两台新执行都保有原定时器 PID，三种钟均未跨过 9 秒到期点，恢复后的钟速与宿主活跃时间一致。

| 实机组件 | Raw Wake 到期 | Managed Wake 到期 | Fork 子机到期 | Hibernate 恢复到期 | `.build/` 日志及 SHA-256 |
| --- | ---: | ---: | ---: | ---: | --- |
| 精确 `fb69434` 安装 | 9.004 秒 | 9.008 秒 | 30.475 秒 | 32.295 秒 | `a08-fb69434-standby-clock-kvm-v4.log` `db7ee7954690017fe2755883e685bd84eacfac2e79c28194840395f97badaf2f` |
| 修复开发 Bundle | 9.002 秒 | 9.008 秒 | 9.007 秒 | 9.008 秒 | `a08-held-clock-kvm-v2.log` `c76456893b9fd25f403d75a0fc0caccbb2031bbf86fbcd2f2757f1043cd9e82f` |

新 reader 的 `SMVCSBY1` 记录源机停驻时长和原 KVM clock/TSC 样本。目标库归一化保存点，Worker 按同一 envelope 校验 Guest Gate，目标在激活前单独扣除本地等待。构建使用带该 reader 的 libkrun 时生成 `RuntimeClockContract/2`。历史 reader 使用 `RuntimeClockContract/1`，其字节摘要可直接用于对照。

### 卷恢复与停机存储实测

原始 `d8cc5d5` 安装件已经拒绝错代际的 `exact` 恢复，`clone_conflicts` 返回新卷并读回旧字节，但公共卷目录漏掉了新挂接。修复后开发 Bundle `sha256:1c1f2091dba9c2f2cc895fb8a525151a6e7dcaa40f32da9936fd21655b6b3305` 将产品卷和挂接随目标激活持久化。两个活源卷在保存点后读到 `live-latest-left-aaaa` 和 `live-latest-right-bbbb`，克隆从 `snap_05079d94389d2ef77374aec8bc1aa160` 读到 `checkpoint-left-abcdef` 和 `checkpoint-right-0123`。两个新卷 ID 分别为 `volume-7748bc734b6331f6c67d1df958b3159e` 和 `volume-5d84547f3696221670cbc5c0a6289c0c`。两个 KVM 的共享只读代际为 `op_8144f92efd01badbd5c68dad4c86d368-head`，Guest `blockdev --getro` 读到 1。

公开 Stop 和离线拓扑试验使用 Stop 开发 Bundle `sha256:4677add0f57d315cf872b93962dd5b666a4360e0e5a4a2530cb9bf08a4fc3d8e`。增长回执的 `block_capacity_applied` 为真，`filesystem_resize_status` 为 `not_requested`。Guest 独立完成文件系统检查和扩容，旧标志文件可读。解挂的持久卷 `vol_e9b1382990eaf3ffca36864038b18b91` 重新挂接后读到 `portable-original-bytes`。

删除后的复测使用开发 Bundle `sha256:84dcd2a3a8da011d87a053af10172bfefab5d5a8b3697551f1186754382ed9d4`。源私有 head `next-043a3ef2259ce52346679524416b63eb` 在 Worker 变为 `deleted` 且实际文件已移除。旧 checkpoint 依赖的 `head-b3e52b12c774547f06e61d6d38f5570c` 仍可用。已转挂至另一活虚机的原 RW 卷保持最新字节，旧点克隆读到保存时的 RAM 和双盘数据。原 RO 代际 `op_8a09a89883bfb81efaa637ea21a2364a-head` 由各自的新挂接引用。

故障复测使用开发 Bundle `sha256:a7769afb760c334a9859235a7fccbcd747fc4a917a3817177e5a27239d3a01a6`。夹具通过隔离挂载中的稀疏 ext4 对象盘触发实际 ENOSPC。旧实现先封存旧 head，磁盘满时留下 Worker `NEW` 和暂存目录。新实现先物化并同步替换盘。Create/Grow 在暂存失败后均记录 `ABORTED`，公开返回 `VOLUME_STORAGE_NOT_APPLIED` 和 `effect=none`，保留原 inode 15、卷修订及 16 MiB 的 SHA-256 `acdfd1236350ce9f7b16f0a5c04cbb73606edbd268d60a3058c686f54e7eaa7a`。冷启动在 Guest 中读到相同摘要。

在替换盘的 32 MiB 文件同步完成后，夹具只对选定旧 head 变为 `0400` 后的 fsync 注入 EIO。操作报告 `recovery_required` 和 `effect=unknown`，seal pin 继续有效，绑定保持停止，竞争快照被拒。Worker 经 SIGKILL 重启后，同一公开 operation 通过 reconcile 继续，保留新盘 inode 19，卷修订增加一次。所有 VMM 已停，夹具使用 Worker 的 `CompleteRecovery` 结束 broker 恢复。新 KVM 读回原 16 MiB 摘要，并确认后继盘实际可写。

原始日志位于 `.build/release-d8cc5d5/`，夹具为 `tests/integration/volume_product_kvm.py`。

| 原始记录 | SHA-256 |
| --- | --- |
| `installed-volume-checkpoint-baseline.log` | `d4de05e26ac6ff7784b41127538e5492f96908fa5862557da90a90bdc034da5d` |
| `managed-volume-checkpoint-fixed-kvm-v2.log` | `10c75bae97e2361def625dfcc05d9886bb0884e9cf05d30ee37b7ebfc4c5e6f3` |
| `native-volume-lifecycle-kvm-v4.log` | `bdb5706900e17fc9004f62f12057a73827ef62d915d248305ad03b39edf986df` |
| `managed-volume-deleted-checkpoint-kvm-v3.log` | `5201aa66e612916c0aee08af39cf8725daa8048230110d6f0d0ac6aeef4d4443` |
| `managed-volume-growth-fault-kvm-v4.log` | `02886381fe5d1cf0e8de5c6af3bf244832c1fa380a6a2f32d9838398dd55adbe` |
| `managed-volume-growth-fault-kvm-v8.log` | `9d6cabe34a019decf8d355b01e0a4b8d158792e32b22cb50f7cbfa58f4db4d56` |

### A15 阻塞 I/O、取消与实际解冻

两次启动均校验 `fb69434` 安装清单中的 libkrun、固件和 identity-helper。每次在两颗 vCPU 上观察三个独立计数器，包含嵌套 PID 命名空间。Guest 对专用 16 MiB ext4 发起文件系统冻结，另一个进程写入并进入内核报告的 D 态。这种等待允许用户态 freezer 进入 armed。三个计数器在 armed 时全部静止，ABORT 后全部恢复。解除文件系统冻结后，原写进程完成写入、fsync 和读回。

两轮都让真实 FUSE 服务收到 LOOKUP 并暂扣回复。未应答的请求分别在 478.48 和 464.67 ms 返回 `failed_thawed/EBUSY`，此时内核仍报告 FUSE 客户为 D 态。另一轮次在准备中发送 ABORT，分别观察到原失败先提交（464.18 ms）和取消先提交（16.33 ms）。查询保留同一终态，三个计数器均已恢复，原 FUSE 请求随后完成。实测记录 `.build/gate-acceptance/fb69434-blocked-io-v4.log` 的 SHA-256 为 `d9e6bb6d8999c062731399848ef871509828382c66ca56187d8b50d79079d05d`，`fb69434-blocked-io-v5.log` 为 `257122ba1ead92b0d2e41e15040ab77298d6966def60172e56116c8ea6854846`。

另两次生产 helper 被终止后均返回 `failed_thawed/EPIPE`，三个计数器在 1.26–1.28 s 内恢复。`d8cc5d5` 的公开 Fork Batch 在普通 Agent 已停止时创建独立子机。两机的服务恢复后，原 System PID 787 和嵌套 PID 1 的计数均从 4 继续，源达 554、子达 527。原始记录为 `.build/gate-acceptance/helper-exit-kvm.log` 和 `.build/gate-acceptance/installed-agent-stopped-kvm.log`。

复现时，静态编译 smolvm 的 `tests/fixtures/freezer-blocked-io.c` 与 libkrunfw 的 `tests/guest-gate/fuse.c`，包含目录为后者所在目录。另将 libkrun 的 `tests/guest_gate/counters.c` 静态编译为计数器。在有 KVM 的 Linux 上执行 `python3 tests/freezer_blocked_io_kvm.py --bundle INSTALL --blocker BLOCKER --counters COUNTERS`。

## 兼容、运维、备份与性能

| ID | 范围 | 已核实结果 | 待验 |
| --- | --- | --- | --- |
| A19 | 冻结双 Bundle、公开历史恢复、开发公开备份与 CPU 合同通过 | `fdd620e` 和 `d8cc5d5` 交叉恢复返回 `E_BUNDLE_UNAVAILABLE`，目标维持 epoch 0。共享 Worker 重启后原 reader 恢复 Guest PID、计数与两盘标记。`d8cc5d5` 与 `fb69434` 同 Worker 的公开 CLI 复测中，旧 reader 缺失返回 `CAPABILITY_UNKNOWN`、`effect=none`，无新增执行，两个持久点仍阻止退役。恢复旧 reader 后，两代 FixedInternal 点继续原任务 PID、RAM 和根盘标记。开发 Worker 校验完整归档和缺块、截断副本，修复 reader 后公开备份达到 `verified`，独立 Worker 从原 manifest 启动并读回 RAM 和根盘标记。便携归档复用增量块并恢复两代，拒绝不匹配的 CPU 数、RAM 大小及真实 KVM Guest CPU 合同。代表语料索引包含原生 OCI、System Native、附加盘、CPU 槽、RAM growth 能力边界与长 Standby。[双 Bundle 安装复测](candidate-d8cc5d5.md#两个冻结-bundle-共用-worker)、[本轮历史恢复与语料索引](statepoint-history-a19.md) | 在包含 reader 修复的完整冻结安装复跑公开备份。 |
| A20 | 冻结基线与开发修复，组件重启和虚拟主机换 boot 已验证 | platformd、Serve、broker 分别退出，systemd 接管前后分别中断。修复后的 Worker 重认原有 TCP 会话、TAP 和 writer，同一块盘录得九轮应答。独立虚拟 Worker 正常关机和断电后，两份旧写授权自动释放，原易失代际标记丢失，同一持久点恢复原 RAM 和盘数据。[组件及整机实测](managed-recovery.md#worker-主机换-boot-后的恢复)、[网络与 DNS](A20_LIVE_NETWORK_RECOVERY.md) | 在含两项恢复修复的完整冻结包复跑。 |
| A21 | 冻结测量、开发实机，部分 | 两个授权 wake demand 共用一次 exact 恢复，仅重新授权通过的请求获新执行 hold。真实 Native/FixedInternal 私网 TCP 可唤醒，授权的短 UDP 首包跨持久唤醒交付，超界数据丢弃；CLI SSH 经 Worker Gateway 可用。 `managed/lifecycle.log`、`.build/packet-udp-full-kvm.log`、`.build/platform-system-gateway-v3.log` | HTTP、TCP-mTLS、SSH 和直接 TCP/UDP 在同一候选上的并发首请求及不重放可能已送达的业务数据。 |
| A22 | 冻结桌面与开发网关，服务修订和活流验收通过 | 桌面单次票据交付真实 framebuffer，Worker 确认原生租约后 API 返回会话。延迟和丢失确认、授权撤回、到期、平台重启均已测，休眠后新授权请求接回原 Xvfb。Guest grant 撤销关闭在途 SSE。f2df146 正向测量组合的 HTTPS SSE 和 mTLS 在 revision 1→2 后切换实际端口，旧流关闭。撤销已建立流的令牌或证书只关闭该流，独立合法流继续。OpenSSH 修订关闭旧流，pidfd 确认旧 VMM 退出，合法新连接唤醒原保存点，从 epoch 1 转至 2 并校验 host key 和 RAM 标记。[服务修订与关闭记录](service-revision-a22.md)、`.build/platform-desktop-worker-lease-a30.log`、`.build/platform-desktop-worker-lease-implicit.log`、`.build/wp23/transactional-proxy-kvm.log` | — |
| A23 | 开发实机，原生引用、并发与发布恢复通过 | Native 用同一 run 跨 Worker 和 platformd 中断备份原 checkpoint、休眠点与独立卷。并发公开删除后 Worker 引用继续保护原数据。暂存缺块和原 reader 缺失均保留已验证备份。关闭及删除旧源后在新 Worker 恢复原 RAM、根盘、独立卷和 Native `/32`。1017 在途备份升级到 1018 后保持两路原选择。六组 ext4 发布和保留中断已通过。既有 Native、FixedInternal 实测还覆盖错误密钥、等长篡改、服务 PID、隔离网络与非管理员注册拒绝。[原生备份与双路记录](#a23-原生备份选择与中断恢复)、[发布中断记录](#a23-备份发布与保留中断)、`.build/platform-networked-backup-native-v3.log`、`.build/platform-networked-backup-fixed-v3.log` | 开发 Worker `1eb5104` 已通过 FixedInternal 交接中断。待同一冻结安装复跑 Native 和 FixedInternal。 |
| A24 | 历史 0.1.6 到 schema 1017、1018、1019，迁移中断与回退通过 | 三个候选迁移中被 SIGKILL 后，19 张旧表及全部行一致。旧二进制拒绝打开新 schema。新旧发行件分别用匹配的备份恢复，原 owner 可登录，旧 control epoch 的公开写入返回 409。另在隔离的 Ubuntu 24.04 KVM 整机上通过安装器分别从 0.1.6 升到 1018 和 1019，整机断电重启后 systemd、owner 和两个版本的备份再次验证。[发行件与整机记录](#a24-历史发行件升级与回退)、[事务夹具](../../internal/registry/deep_migration_test.go) | 最终发行锁确定后按该版本复跑。 |
| A25 | 两个冻结安装、同一 Worker、两种 Serve 启动顺序 | 在同一 Worker 上交换 `fb69434` 与 `86dae32` 的启动顺序。两组均保留原 slice 限值和两台受保护 VM。并发启动、Fork、Restore 的六个请求受同一预算限流，逐 VMM 取得 PSS、私有与共享映射计数。[实测与日志](#a25-两个冻结-bundle-的共享预算) | — |
| A26 | 冻结安装，官方与定制实机通过 | Chromium 创建默认 Native 的停机 System，并禁用状态 `unknown` 的 Fork batch。TS 挂卷后冷启动，CLI Standby/Wake 保持同一执行，TS 从完整 VM checkpoint 恢复原 RAM 标记与 `/dev/vdb` 字节，Python 批量创建两个独立 writer 和新 Native 地址。默认变更只影响新建机器，CLI 接受 Fixed 覆盖。官方 v1.37.1 的真实 OCI 保持 Pause/Resume 与 `managed_clean` clone 语义，实例间写入彼此独立。浏览器分开显示电源、Guest 管理、工作负载、网络与服务状态，并保留官方兼容动作。Python Standby 得到 `CAPABILITY_UNSUPPORTED`。 `.build/platform-a26-d8cc-installed-v2.log`、`.build/a26-d8cc-evidence/` | — |
| A27 | 冻结安装及开发实机，部分 | 本地 ext4/KVM 对照了冷启动、双分支和三层递归，有界 dirty、首次/重复 durable restore 与 16/4/0 读通道。另用 `fb69434` 进行 16 次受管 Fork，逐 vCPU 在内核记录退出与进入，双分支共同停顿 p50 为干净 5.071 s、写脏 5.081 s。首任务、PSS 和双盘正确性均保留实际观测。[WP-22 测量与原始样本路径](performance-wp22.md) | 本地 96 次 rebase 与预取、24 次共享页缓存对照和 KVM/设备写并集已通过。网络存储和更高并发的块读通道仍待测量。 |

### A23 备份发布与保留中断

公开 API 提交第二次备份时，第一份已用发行版 sandboxctl 校验。夹具在私有 ext4 备份目录订阅 Linux fanotify 打开事件。它用 pidfd 在指定文件操作途中终止 platformd，并通过正常启动入口重启。每日策略保留一份归档。旧平台 c011476 在目录改名后留下暂存标记，在新回执已提交后中断则要等下一个备份周期才会继续回收旧目录。

当前平台在公布已验证回执前同步父目录，重启时重新验证已发布的归档。备份保留流程在启动时核查待处理的回执，并在删除上一份归档前确认保留份数有足够的可验证副本。六组复测覆盖暂存、已改名但回执未提交、已验证但尚未开始清理、旧目录进入清理、旧目录已删除但回执尚未更新，以及重启时最新归档遭到单字节篡改。每次恢复均保留同一运行 ID、暂存数据库 inode 和内容摘要。损坏场景先独立验证上一份仍可恢复，修复最新副本再完成保留处理。六组 ext4 实测日志 `.build/wp23/a23-publication-ext4.log` 的 SHA-256 为 `f21203617b6c7d3bd0cc7068ef4f2c538c8e43882bfb8e4ecc0ad01ec1ab5bf7`。[备份中断夹具](../../tests/integration/backup_publication_crash.py)

### A23 原生备份选择与中断恢复

自有 FixedInternal System 在开发 Worker `1eb5104`（开发 Bundle `0b9e399f082e7b021a4cd078711b5bcd46843bdbb8c08b1f6f4bae923e4232b5`）完成了同一组工作。源停止并删除后，新 Worker 在原生归档中新建 catalog。恢复前的 bindings、TAP 租约、包唤醒规则均为零。恢复后新的路由域获得私网地址 `10.194.0.2`，Guest 仍使用 `10.0.2.15/24`，RAM、根盘和独立卷的标记相符。日志 `.build/a23-1eb5104-development-fixed-handoff-v1.log` 的 SHA-256 为 `dddff22c705c3e5f9277b2986fc59c118561407f5e274d4fb241871e3858ea51`。

在真实 Debian KVM 上，认证的公开请求选定 checkpoint、当前 Hibernate 点和独立卷代际。Worker 离线时，平台先持久保存选定的目录快照。已有休眠点的公开 Delete 和显式释放受围栏。公开 Wake 及再次 Hibernate 更新了当前恢复点，原休眠根继续阻止 GC，原 run 保持选定此前的点。Worker 确认引用后，暂存阶段成功删除公开 checkpoint 和独立卷快照，四次直接 GC 仍被拒绝。

在第一个原生归档成员打开前终止 platformd，并在其重启前终止 Worker。平台等到同一 Worker 重启后才能继续。Worker 仍为 `recovering` 时，备份以原 run、原目录 SHA-256 `a23e3d8756048661138b8ee224d2b7f253c4c50b8d23a186419e05e4475cdf82` 和原 closure SHA-256 `1fd2ab42e7281abd2df673e66e8a00d95c704eec2dc1af4ca5e4a44ab0b0b7e0` 完成。随后分别在 Worker 已确认引用的暂存阶段隐藏一个盘文件，以及在准备阶段隐藏原 reader 清单。两次运行均返回 `NATIVE_CLOSURE_UNAVAILABLE`，先前两份备份的清单摘要保持不变，原合格备份再次通过校验。

公开 Stop、Delete 确认源 VMM 退出后终止旧 Worker。用归档初始化独立 Worker 与 Serve，从原 checkpoint 启动另一个 Debian 执行。新执行在 epoch 1 读回原 tmpfs 和根盘标记 `26532ec1137b1a30f4939ac5505996af900d`。独立卷在 4096 字节偏移的内容与原 Guest 写入相等。目标网络使用新路由域和 attachment，Guest 实测 `10.194.0.2/32`。

开发 Bundle 为 `sha256:99509478496eb4d6711c96bdbff8bf08fb44f525e602865e1b152efc65b51399`，Worker 和 platformd 的 SHA-256 分别为 `f37f8ecab855c8eb178d27b59750bc17c3519fa41cfd396a3976b395d9a0f9e1` 与 `c512bcbee37bb5d8c20c53853532ae65a3a68dd08d707a0c98eeeabbe546a1f3`。备份输出和 Worker 目录使用私有 tmpfs，保留正常存储准入。原始 KVM 记录 `.build/release-d8cc5d5/native-backup-restart-a23-v6.log` 的 SHA-256 为 `9efa60802ec756c8f36122873dada76dfc7563dd9fa5db35c488003fb22f88ab`。[原生备份复现夹具](../../tests/integration/native_backup_restart_kvm.py)

两个真实 Serve 共用一个 Worker，各自保存一份独立卷代际。schema 1017 的平台在第一路 Worker 提交备份引用后被终止。schema 1018 随后接续同一公开 run，以 SHA-256 `b3775d658ee910bb342412791592c4611a0a34ce799468e71854fd16e6b6d6b5` 的原 schema 1017 数据库验证两路归档。第一路 Worker 的原 closure 字节相等，完成验证后两路引用均已释放。

同一安装随后公开提交卷快照删除并使其停在已 dispatch 的状态。并发备份保持 `running`，目录快照没有提交标记，Worker 没有建立备份引用。恢复 Worker 后，原删除成功；同一备份运行选定仍保留的两个代际并通过核验。日志 `.build/release-d8cc5d5/native-backup-legacy-two-routes-a23-v1.log` 的 SHA-256 为 `e864b875cc3978c0b0091cf14854bd322fee8c25aba6b041d4974b58f283a4d5`。[双路及卷删除夹具](../../tests/integration/native_backup_restart_kvm.py)

### A24 历史发行件升级与回退

旧包使用归档 `sandbox-platform-0.1.6-linux-amd64.tar.gz` 中的 platformd 和 sandboxctl，归档 SHA-256 为 `7949b70b2cedf3d6899107e653c8b17a80a96e0a22efe9112519b7f4752a4c3f`。候选 `0.3.0-dev.c011476` 由 `make-release.sh` 从干净提交 `c011476d0f533d606dd3c6cd09ad2f0c532bdfa9` 生成，采用目录中已登记的默认 Debian 模板。安装器 `--plan` 核对了发行文件和官方 1.37.1 运行时归档。候选发行清单 SHA-256 为 `6948825a9a5990f4a9657de5be4a7bebaedfc9d9397a3b04ac4e47eab959ae4e`。

旧 platformd 在隔离 ext4 目录首次启动，创建真实 owner、安装标识和 schema 2 数据库。夹具加入一台机器及其历史 pause 记录，使用旧 sandboxctl 创建并核验备份。迁移进程写完首个 4096 字节 WAL 页和下一帧未提交的头部时，夹具通过 pidfd 发送 SIGKILL。原文件 inode 保持，19 张表的定义和全部行与故障前一致，完整性和外键检查通过。旧 platformd 随后接受原 owner 登录。新 platformd 成功升级后保留该 owner、安装标识、控制代次和 pause ID。旧版本再次启动时报告 `unsupported registry schema version 1017`，升级后的数据库逻辑摘要保持一致。

新 sandboxctl 原位恢复历史备份，并生成可验证的升级后回退副本。另一独立目录由旧发行件恢复相同的 schema 2 备份。两端的恢复流程分别轮换控制代次，原 owner 均能登录。带原代次的机器修改均返回 `409 CONTROL_EPOCH_CHANGED`、`effect=none`。旧目录恢复完成后，再次核验升级后的安装。原始日志 `.build/wp23/a24-registry-crash-ext4.log` 的 SHA-256 为 `ecf63c52568eab5ac38129e4463d13920731f181a864233da978279895b88e74`。[端到端夹具](../../tests/integration/registry_migration_crash.py)、[测试进程 WAL 切点](../../tests/fixtures/migration-wal-cut.c)

候选 `0.3.0-dev.1452d53` 的发行清单 SHA-256 为 `35392e8523ac61ba551b9cefa18cd522002877cc1e01b92b46b04167921e3d0a`。同一 WAL 切点重测 schema 2 到 1018，19 张表、旧 owner、原 pause ID、两端恢复和旧代次围栏全部通过。记录为 `.build/wp23/a24-schema1018-registry-ext4.log`（SHA-256 `c74b6338ea44fdcb2292f6f2d777c49de1f00ce84f0d9c5ce5c6df84e4001bb6`）。

整机使用 QEMU 8.2.2、嵌套 KVM 和 Ubuntu 24.04，候选与旧包从只读数据盘提供。旧安装器启用官方 Serve 1.37.1、状态 socket 和独立非 root 平台服务。旧 CLI 进入维护、生成备份并用公开升级命令切换到新程序。官方 Serve PID 422 跨平台升级保持，跨 schema 的二进制回退被安装器拒绝。退出维护并核验之后同步 Guest 文件系统，随后终止该 QEMU 进程。同一虚拟磁盘再次启动，宿主 boot ID 由 `829a9994-6969-4e93-b531-af7d8c1fd431` 变为 `f917746d-ff2a-44cd-a1b2-92182af29c16`。systemd 自动拉起两个服务，原数据库 inode、安装标识、control epoch 和 owner 均匹配，两份备份的校验结果逐字节一致。此切点覆盖已确认并同步的升级。原始记录为 `.build/wp23/a24-systemd-powercycle-1018-v4.log`（SHA-256 `4cf526a956feecf213fb786c4a6594d5cf59cf71baccae05d7c6bb5471f5325a`）。[整机夹具与所需输入](../../tests/integration/release_host_reboot.py)

升级到 `0.3.0-dev.5c4d060` 的 schema 1019 时，独立复跑了两组测试。候选发行清单 SHA-256 为 `c49ff28f52b40098bbb4a61d33cf981bd9445c9ec0a9823ef1990afa4618baf5`。迁移中断及新旧发行件分别恢复的记录为 `.build/wp23/a24-schema1019-registry-ext4.log`（SHA-256 `3cc82df393194944e24a173b32cb36b229464c7d695929839063e928b775e3f0`）。整机的官方 Serve PID 421 跨平台升级保持。同步后终止整机，新 boot ID 从 `0b4f0997-c02a-43fa-84ae-b3e3c07f5b91` 变为 `c224be91-ffae-4401-815f-eb4e797be616`，原 owner 和两份备份仍通过。记录为 `.build/wp23/a24-systemd-powercycle-1019.log`（SHA-256 `b7df463cb2640c6ce9fc86ed4f62a73c23a80a882bb8e178f1999c671577b7ef`）。

候选 `0.3.0-dev.83b3d29` 再次通过旧安装器升级和整机掉电重启。公开升级过程中原官方 Serve PID 421 持续运行。新 boot 从原 ext4 目录启动后仍为 schema 1019，原数据库 inode、安装标识、控制代次、owner 和两份备份清单均通过核验。发行清单 SHA-256 为 `b2b253dc1fc6d463293070a7caff189c26c41f11b547fcf3110fb37ba6bac7a4`。整机记录 `.build/wp23/a24-systemd-powercycle-83b3d29.log` 的 SHA-256 为 `8de0351f5525697aeede82a4348277308f8f552cbf5ad1d2363f5a471fd2fc9d`。

平台构建 `8164e99` 与历史 `0.1.6` 在 ext4 上复跑了同一迁移中断。两帧尚未提交的 WAL 共 4,176 字节，原 19 张表及其数据完整恢复。新平台随后迁至 schema 1019。新旧发行件各自恢复同一备份，原所有者分别重新登录并通过公开接口读到新的控制代次。两端保留原暂停记录。夹具参数 `--owner-readback`，日志 `.build/wp23/a24-owner-readback-8164e99.log` 的 SHA-256 为 `1ccbfac6f843773fb5cf18430e8c7675fac59fae60d744ad7d06d67f40c16ee4`。

### A25 两个冻结 Bundle 的共享预算

在两个独立的 systemd 管理器中分别验证两种 Serve 启动顺序，都使用 `fb69434` 安装目录中的同一 Worker（SHA-256 `0c6454b969bd304fb98668ff963b9dc4a2b3703acd4228ea9762b8cd326f7d9c`）。各 Serve 的 reader、Bundle、运行二进制摘要和 vCPU 进程的 cgroup 均实测。共用账本记录 3 vCPU、896 MiB 和 32 MiB 存储。每次启动第二台 VM 时，slice 的常驻用量至少增长 16 MiB，两个 Serve 均保留原有 cgroup 配置。

每组均创建两个持久 checkpoint。并发导出争用同一存储额度，完成与取消分别归还一次预留。压力下，两路 Serve 各收到一次正常启动、一次双子 Fork 和一次从自己 checkpoint 恢复的请求。两次各六个并发请求全部受限流，受保护的原 VMM 存活，分配额与请求前相同。

| 启动顺序 | Bundle | VMM PSS MiB | 私有映射 MiB | 共享映射 MiB |
| --- | --- | ---: | ---: | ---: |
| fb69434 → 86dae32 | fb69434 | 46.07 | 42.51 | 9.45 |
| fb69434 → 86dae32 | 86dae32 | 47.12 | 43.52 | 9.54 |
| 86dae32 → fb69434 | 86dae32 | 42.83 | 39.29 | 9.41 |
| 86dae32 → fb69434 | fb69434 | 45.07 | 41.38 | 9.75 |

各 VMM 的 `smaps_rollup` 提供上表。共享映射按进程分别观测，同一共享页可能出现在多行。两组 slice 的 `memory.current` 分别为 181,919,744 和 162,844,672 字节。原始记录为 `.build/wp23/a25-two-orders-forward-v1.log`（SHA-256 `2df07907b602a430684c5c9e5b26ce13cdee90c441b6fc24de1d21c1bcd4f19c`）和 `a25-two-orders-reverse-v1.log`（`8547f3384df5e447254d81ab7253087f2f053cbb48f6712ece2a9f42968775db`）。

### A26 同平台的双运行时

A26 由同一个 `platformd` 进程连接冻结 `d8cc5d5` 的私有 Worker Serve 和官方 Serve，两套运行时各用独立状态目录。冻结安装包摘要为 `sha256:c5070578cdf0d4fd01cc120721515c169e9297a3f624803def398996c0e249b5`，平台、浏览器资源和客户端基于锁定的 `9eb18ae`。官方归档 SHA-256 为 `f8d5d1aaa198c7b9e676fa4365998e22fc9051a2199b208741be5f38fef5e404`，解包的九个文件逐一与 `packaging/runtime-manifest.json` 匹配。官方 clone 使用发行版状态 helper 的 `SO_PEERCRED` 与固定名称校验读取实际 CLI 的 `restart_policy`、绑定标签与状态，夹具将 helper 的 UID 和路径设为本次隔离实例。平台状态使用独立的 4 GiB 临时文件系统，运行盘置于外部 ext4，磁盘准入沿用发布默认值。复测日志 SHA-256 为 `98333564ae728ad43a85e89ee74a02a728519d260d6544f738d9c88ba3441cfe`。

## 自动生命周期

| ID | 范围 | 已核实结果 | 待验 |
| --- | --- | --- | --- |
| A28 | `86dae32` 冻结安装和更新 Worker，同机实测 | 公开策略等待 Worker 回执时显示 `pending`。同一 Debian 逻辑实例分别核对 Standby 单开、Hibernate 单开、共用 idle 起点的两阶段、停驻证据失联后的阻塞、raw 停驻 profile 的阻塞和旧 `idle_policy` 仅休眠。Hibernate 恢复的新执行和同一执行的两次短 Standby Wake 都获得三个新 collector 会话。迟到报告被围栏，两路公开详情在 Worker 失联时一致显示 blocked。另已核对全局默认、覆盖与批量应用。 `managed/lifecycle.log`、`.build/wp10/debian-lease-and-protected-fork-kvm-v2.log`、`.build/a28-idle-policy-kvm-v6.log`、`.build/wake-collector-kvm-v2.log` | — |
| A29 | 冻结安装、开发实机，代表场景通过 | Guest 等待 TLS SSE 超过 idle 阈值时仅有 `hold:credential_proxy`，流结束后同一执行自动 Standby。私网 Guest 直连 HTTP SSE 时保持源出站和目标入站证据。Gateway OpenSSH 静默 PTY 有 `hold:service_access` 和真实连接证据，独立 Guest Relay 前台 Exec 静默 12 秒仅有 `hold:guest_exec`。等待客户端的桌面会话仅有 `lease:desktop_session`。新版 System Agent 用公共终端 API 验证权限、断连时的 Worker hold、按偏移重接及退出码。各项活动结束后，仍有监听 daemon 的同一执行实际进入 Standby。 `.build/wp23/transactional-proxy-kvm.log`、`.build/guest-direct-sse-kvm.log`、`.build/platform-desktop-worker-lease-a30.log`、`.build/platform-guest-silent-exec-a29-v2.log` | 在下一完整冻结安装中复测含 `system-exec-stream-v1` 的 System 镜像。 |
| A30 | 冻结 Worker 与公开平台，自动候选和保护排序通过 | 排队中的压力休眠分别被同一 Worker 新确认的 Access hold 和 `keep_running` 取消。公共 `keep_running` 在空闲 Standby 排队期间确认，Worker 记录取消且未提交原生操作。另一次已提交的 Standby 完成后，新桌面 Access 在原 epoch 唤醒并取得正式租约。获确认的 v2 续约是原到期时间之后唯一 blocker，释放后进入 Standby。两个并发公共请求与恢复 monitor 推进同一运行租约，连续两轮均只产生一个有效租约。 `.build/worker-pressure-kvm-v9.log`、`.build/wp23/lease-serial-installed-kvm-1.log`、`.build/wp23/lease-serial-installed-kvm-2.log`、`.build/platform-desktop-worker-lease-a30.log` 新增 4 组自动 Hibernate 实机。`keep_running` 与所有者 v2 租约分别在 `STOP_COMMIT` 前保留原 epoch，在提交后保留原保存点并从 epoch 2 继续。合法租约在当前执行续租，越过初始 TTL 和空闲期限仍阻止自动睡眠。每组实际只有一项自动候选。[四组保护排序](#a30-保护与自动休眠提交) | — |
| A31 | 冻结安装的八组私网切点、修复平台的公开 HTTP 与 API 切点通过 | Native、FixedInternal 各从 Running 和 Standby 在 STOP 提交前后复测。服务与管理 API 并发进入时，提交前撤销休眠并保留原 epoch，提交后共同精确恢复同一 StatePoint，各自取得新执行的 hold。所有者撤销一个在途连接后仅另一个请求产生业务回执。一次 Worker 控制读取超时后仍返回相同回执。[服务与管理 API](#a31-并发服务与管理-api)、[私网安装复测](candidate-d8cc5d5.md) | — |
| A32 | 冻结 Worker、开发平台与 Chromium，所有者生命周期通过 | 实际公共 v1 租约过期后保持 `suspect`，v2 到期后为 `released/expired`。Chromium 使用同一所有者会话，分别显示逾期保护继续、到期保护结束及操作按钮状态。Guest 任务服务停止时，页面显示活动状态未知并阻止自动睡眠。所有者用原 v1 租约完成续租和主动释放，始终在同一执行。夹具 `tests/integration/lifecycle_lease_ui_kvm.py`，记录 `.build/wp23/a32-fb69434-browser-owner-lease-v1.log`（SHA-256 `495a41c05334cfcad9e3ed43d882067de28842f8a39dd9ca64c53f2b490112a5`）。 | 子机或旧执行的令牌重放按要求跳过。 |
| A33 | 同 Bundle 和双 Bundle 冻结安装，压力与共享引用通过 | 两路 Serve 共用单个 Worker 账本和隔离 slice。六个并发启动、Fork、Restore 在候选全受保护时被拒绝，两个 VMM 继续。两套冻结 Bundle 并存时仅低优先级 Guest 休眠，实收 62,935,040 字节。同 Bundle 组实测六个共同 RAM inode 在第一子机退出后保留，第二子机退出后 Worker 关闭托管 FD，活父机继续持有。父机完成休眠后 `shmem` 为 0，`memory.current` 总降 111,386,624 字节。[d8cc5d5 安装复测](candidate-d8cc5d5.md) · [fb69434 自动 balloon](#a33-实际-balloon-反馈与预算) | — |
| A34 | d8cc5d5 冻结安装，重启与唤醒恢复通过 | platformd 强制退出后仅释放该进程的连接，独立运行租约保留，迟到的旧申请被拒。Worker 经 SIGKILL 后从同一安装件重启，原 VMM 和已确认策略继续，旧 PTY hold 进入 `suspect`，重放的旧活动序号无法解除 `unknown`。丢失应答的唤醒与另一请求合并为一次精确恢复，重授权只给获准请求授予新执行 hold，旧 epoch 无法续租。持续压力下 20 秒唤醒宽限得到验证。[d8cc5d5 安装复测](candidate-d8cc5d5.md)、[fdd620e 安装复测](candidate-fdd620e.md) | — |
| A35 | `fb69434` 冻结安装，运行中的单子 Fork 和双子批次均通过 | 源关闭自动休眠、开启 keep_running 并持有有效租约。Guest 已收到 HTTPS 模型流的首个 SSE 事件，Worker 确认代理连接仍有活动保护。源和各子分支保留同一原进程、内存 nonce 和首事件，原业务请求只到达一次。各分支用独立授权取得互不相同的后续响应，子机继承策略和空租约列表。[原始运行与请求计数](guest-delegation.md#active-model-streams-during-fork) | — |
| A36 | fb69434 冻结安装通过 Stop、首桌面和禁止失败冷启，开发 Worker 的显式 exact 重试通过 | API 隐式唤醒策略分别约束 Running 与 Hibernated，私网 TCP 与已有桌面分别按其服务策略唤醒。冻结 Stop 在普通 Agent 停用时确认 writer、旧执行和待处理唤醒受到围栏，重新冷启读回根盘、挂载卷和裸盘。冻结首桌面从 epoch 0 空槽并发启动一个执行，复用租约并回收确认期间撤销的授权，真实传输 800×480 画面。两种保存恢复故障都保留同一 Hibernated 保存点，冻结 Worker 拒绝冷启。一个已发给失败目标的 writer grant 完整释放后，此版的显式 exact 仍返回 `VOLUME_GENERATION_MISMATCH`。修复后的 Worker 在同一测试中确认失败目标退出、授权释放且尚未发出身份 BEGIN，随后从原保存点恢复 RAM。[开发修复与同夹具对照](candidate-fb69434.md#保存唤醒修复的开发复测)。[同包 Stop、首桌面与失败原记录](candidate-fb69434.md#安装后公开操作) · `.build/wp10/debian-lease-and-protected-fork-kvm-v2.log` · `.build/platform-desktop-worker-lease-wake-policy-v3.log` | 用修复后的冻结运行时验证围栏失败目标后的 exact 重试，确认原保存点、卷和 RAM。 |

### A36 保存点唤醒失败后的显式重试

`managed_system_vsock.py --case saved-wake-exact` 使用只读的开发 Bundle `0b9e399f082e7b021a4cd078711b5bcd46843bdbb8c08b1f6f4bae923e4232b5` 和 A31 记录的平台二进制。丢失本次保存点的索引时，所有者请求返回稳定的恢复错误。随后在一次恢复已取得 writer、尚未开始 Guest 身份交接时终止该目标。Worker 回收原 writer，保留同一 StatePoint。所有者显式启动后从该点恢复至 epoch 3，原 RAM 标记、逻辑根卷 ID 和单独写入根卷的随机标记均相同。记录为 `.build/wp23/a36-owned-exact-retry-1eb5104-development-v1.log`，SHA-256 `a823414f2adb88dbe4df5442117a27e21070faa5380df915af396049a1ad0564`，新冻结安装待复测。

### A30 保护与自动休眠提交

只读 `fb69434` 安装使用独立 cgroup 和 512 MiB System。所有者的设置与租约走公共 API，夹具从同一个 Worker 日志读取自动候选的真实阶段。`arming` 收到保护的两组均取消候选并保留 epoch 1。已写入 `stop_committed` 的两组完成原休眠。配置组通过显式启动接回保存点，运行租约组凭合法访问自动接回，再在新执行上续租和释放。四组均读回原 `/run` 标记，跨过原 5 秒 TTL 与 2 秒空闲期限时各有对应保护。入口为 `tests/integration/lifecycle_idle_kvm.py --protection keep-running|lease --side before|after`。

| 保护 | 提交前日志 SHA-256 | 提交后日志 SHA-256 |
| --- | --- | --- |
| `keep-running` | `15a4f7098451851b28d5b6fb4e373862f8ac2e2248fe9ea8ab15f6bd6cd7655f` | `69eccfc110e804bba6e8c70d6f18b20a0664b6ad9e4481594d66790f12777508` |
| `lease` | `c40afc53c643048ce435d7543fce04105c28f1d7b1e8e1dcf11c46d0b55a4d60` | `6fb0d5696650e8627d0b7ffe0c6ead590074e6430ec8967571ce7ef001125e44` |

日志位于 `.build/wp23/a30-owned-{keep-running,lease}-{before,after}-v1.log`。

### A31 并发服务与管理 API

两条已授权请求从同一屏障放行。HTTP 服务向自己的 Guest 追加一条业务记录，管理 API 通过 Guest exec 追加另一条记录。测试使用只读冻结安装 `fb69434`、System 镜像 SHA-256 `f7b0611ebb21af630addeddf95717aae284f7aa44b4abe2ee7c3933919570b02` 和修复后的平台二进制 `ecfc92b523d63234163618ca8d04a788db57853d844891c430f03f1d585a8306`。原平台曾在 Standby 切点返回 HTTP 500。一组单独延迟 Worker 应答的对照记录了两条 `GetLifecycleStatus` 读取超时。修复后每次控制层重试沿用原执行、请求键和载荷，业务仅在持有当前执行的保护后交付。

| 初态与切点 | 实测结果 | `.build/wp23/` 日志与 SHA-256 |
| --- | --- | --- |
| Running，STOP 前 | 取消休眠，epoch 1，两条业务各一次 | `a31-acknowledged-running-before-v4.log` · `62961ca85b610cc5c1eb5432f37e7dc1ad12c876c0b38e06639da41bd22bbfa5` |
| Standby，STOP 前 | 连续三次取消休眠，原执行唤醒，两条业务各一次 | `a31-acknowledged-standby-before-v1.log` 至 `v3.log`，第三次 `ce0597e563477dca6497e8a51643d57cb997ef611f67ed866b296b87e645b668` |
| Running，STOP 后 | 一次精确恢复至 epoch 2，同一唤醒发放两个 hold | `a31-acknowledged-running-after-v4.log` · `e862d6d41f514de7dffd35dbe8d5396f8e89e0efffe815299f3bf2659f43b745` |
| Standby，STOP 后 | 一次精确恢复至 epoch 2，两条业务各一次 | `a31-acknowledged-standby-after-v4.log` · `20f303641e42c62e223dbeea4bea98c0a68efe390e6ce95ca64cbe46b45d358f` |
| Running，STOP 后撤销在途服务 | 已提交的 HTTP 返回 401，Guest 无该业务记录。管理 API 单独取得 hold 并完成 | `a31-acknowledged-running-withdraw-v4.log` · `9db32544ddd4ac13fb1485106f39e3f123bdceb14360fd1caa529e07f0a75e2b` |
| Standby，保存完成后人为延迟本次 Worker 6.5 秒 | 跨越 5 秒 RPC 超时，HTTP 200。一次精确恢复，两个新 hold，两条业务各一次 | `a31-owner-control-timeout-stable-v3.log` · `c61b6b8f18640e163023e570edf0528c982f02fb561e84cd4bbc86fcda33b89d` |

冻结平台 `c095c4f` 的二进制 `c7045e98f56f09805774ea0c785f7773a172831042e88bd13dca6168875659ba` 同样通过 6.5 秒延迟组。日志 `.build/wp23/a31-owned-final-platform-c095c4f.log` 的 SHA-256 为 `2483c770907a0b135b58fbeba8c8beb01a7a11d40dc010bcf18c6c3a68f38a0f`。[公开切点夹具](../../tests/integration/lifecycle_access_cut_kvm.py)

### A33 实际 balloon 反馈与预算

`fb69434` 安装目录的 Serve 和 Worker 启动真实 Debian，Guest PID 1 为 systemd。两台独立 VM 分别保留和解绑自己的 virtio-balloon 驱动，各运行两次，第二组交换启动顺序。驱动绑定时，正常 balloon 扩到 16 MiB 再退回 0，VMM 两次均确认实际值。Worker 在隔离 slice 发生压力时自动请求空闲页，每次得到 415,236,096 字节的 Guest 报告。宿主 `memory.current` 第一轮前后均为 122,499,072 字节，第二轮从 122,138,624 到 122,400,768 字节。Worker 两轮均继续保留 512 MiB 配置预算。

同样的压力下，解绑驱动的两轮都在等待限时后报告 `timed_out` 和 0 字节，配置预算仍为 512 MiB。各轮自动决策 ID 与 Worker 压力回执相同。第二组记录 `.build/wp23/a33-fb69434-balloon-bound-v2.log` 的 SHA-256 为 `780db4b9ec701e80463a66baf4c878937ce9a13bbf22e8d10c85e68569b4f444`，`a33-fb69434-balloon-unbound-v2.log` 为 `6a8435155a474e89de3b47dc36ece7664e9a6521ee5e71d37e3ce07d28f76a31`。

当前正式发行门采用 [验收定义](ACCEPTANCE_AND_OPERATIONS.md#10-发行完成清单) 的能力声明规则。上表的待验项仍需要对应的实验或明确的发布范围决定。

同机 A28 测试以只读挂载使用 `86dae32` 安装，Bundle 摘要 `sha256:24e874e4bdddbb18c3bfb93e4562e720953bac6ae77626618276a83f0fd277d1`。同一 Debian 根盘 SHA-256 为 `065e82d9053c8a4a1ed6ba9030ccd816b3791f498576648a9a11010ee33b281d`，平台和 Worker catalog 使用独立临时文件系统。两阶段分别使用 4 秒及 30 秒阈值，来自同一个观测的单调 idle 起点。Guest 报告过期后仍有确认的 managed hold，Worker 完成休眠。另一次待机完成后暂时移开该执行的原生控制 socket，平台显示 `blocked` 与 `unknown:native_inventory`。恢复 socket 后，原执行继续停驻且仍因 `unknown:guest` 被阻塞，直到显式 Wake。真实 raw 停驻通过 `unknown:standby_hold` 保守阻止自动休眠。旧请求将已启用的压力回收改为 `disabled` 并仅安排 Hibernate。各次显式恢复核对了原 RAM 标记。 [复测脚本](../../tests/integration/lifecycle_idle_kvm.py) `.build/a28-idle-policy-kvm-v6.log`（SHA-256 `17f2d425f880ff4578cea29ca9767b617f3c3114d543f088ccd4f48c1ee563ce`）。

短暂停驻的追加实机使用相同安装和 Debian 根盘，在独立挂载命名空间将 Worker `7e9e3a7` 与重新测量的 manifest 覆盖到只读安装路径，得到开发摘要 `sha256:27b857359bd8358f0051cee25061d5d0f98c7fd80a0a5704799ba49654aaa99a`。两次 Wake 均在 Guest 报告 TTL 过期前返回同一执行，Worker 清掉三份旧报告。复测分别拒绝了旧会话、未登记会话的旧代际报告和带新代际的旧会话，随后平台以三个新会话取得确认。Worker 停止响应期间，机器详情及 policy GET 均显示 `blocked`、`unknown:worker`，恢复实测后均显示 `applied`。同一 fixture 从 1017 平台保留已入队但尚未确认的旧请求，升级到 1019 后由 Worker 确认禁用压力回收，原操作正常完成。1019 只纠正未指定压力回收的 legacy 设置。历史 Worker 缺少代际字段时继续使用其原有私有报告合同。 [复测脚本](../../tests/integration/lifecycle_wake_collector_kvm.py) `.build/wake-collector-kvm-v2.log`（SHA-256 `be298cf628b750e159039a8846b0e26fd4ad8b78b3aecb5973156e03d53faadc`）。

公共可重连 Exec 实测使用冻结 `d8cc5d5` 安装的 Worker 和 VMM，Bundle 摘要为 `sha256:c5070578cdf0d4fd01cc120721515c169e9297a3f624803def398996c0e249b5`。正例在独立不可变 Debian 根盘中安装 `f027f2d` 构建的 System Agent，根盘 SHA-256 为 `131c58f2786ae5e01cb76f646879dbb6dc708d51a5c870ceb2ae1ee9bbe59360`，Agent SHA-256 为 `b8d5ecbc74dcc03d147f612849e651c1dad3ec9c6c659107863b2551caa812cf`。机器范围的管理令牌创建了 PTY。连接断开并静默 5 秒后，Worker 仍报告 `hold:api_access`、`guest:exec`、`platform:exec` 和 `platform:stream`。新单次票从原输出偏移继续，stdin 重放只生效一次，窗口变为 31×101，退出码为 17。非 TTY 回传分离的 stdout 和 stderr 及退出码 9。终端读者凭据撤销后，其连接关闭。平台进程被强制终止后，Guest 回收了对应命令 PID，旧会话进入 `session_lost`，旧输入被拒，只有监听 daemon 的原执行再次进入 Standby。保留原冻结 Debian 镜像的反例在发送命令前返回 `NOT_SUPPORTED`，Worker 释放了访问保护。 [复测脚本](../../tests/integration/native_exec_kvm.py) `.build/platform-native-exec-reconnect-kvm-v4.log`（SHA-256 `37951fcde444bd9c937c18dfd249cc2a7e72d7d9edf6dbed495f6eb5fda41a3f`）、`.build/platform-native-exec-legacy-kvm-v2.log`（SHA-256 `f223b3c8d3086328c9daae87d58166e9f672d1bc3e15a21f173f8ba0ede86e82`）。

同一平台构建回归官方 Serve 1.37.1。OCI 命令默认在 `/workspace` 运行，同键重放复用原 Exec。新票从输出标记之后续读并取得退出码 13。 `.build/platform-stock-exec-compat-kvm.log`（SHA-256 `cf0aa3ebc4c25c39b27bfd968d53e6b0eab74764fe16bf08dbdf9093f9c3e55d`）。

桌面租约实测使用冻结 `fdd620e` Worker 和平台 `b0de58d`。夹具先暂缓送达桌面租约申请，已确认的 `hold:service_access` 在申请期间保护 Running，平台尚未发布会话。丢失成功答复后，相同幂等键得到同一个确认租约，Worker 只记录一次申请。答复送达前撤销凭据，平台拒绝发布并释放该租约。平台进程被强制终止后，独立 Worker 租约继续生效，新票据可连接原 Xvfb 进程。另实际从 schema 1016 升至 1017，历史会话保留原 ID、到期时间和执行身份，取得同 ID 的 Worker v2 租约后才发新票。 `.build/platform-desktop-worker-lease-a30.log`、`.build/platform-desktop-worker-lease-upgrade-v2.log`
