# WP-24 发布验收台账

2026-11-26 以 `fb69434` 和 `c011476` 冻结组合续验 [A01–A27](ACCEPTANCE_AND_OPERATIONS.md#2-核心能力完成矩阵) 与 [A28–A36](ACCEPTANCE_AND_OPERATIONS.md#122-新增必要验收)。每行记录观测到的结果和余下的组合验收。测试在隔离的 Linux x86_64 Worker 或本地测试环境进行。

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
| A06 | 冻结测量、开发实机，部分 | 冻结 OCI 在 child 激活前失败，重试保留已成功 child 和原 capture。开发 Worker 记录 STOP_COMMIT 重启后的原执行退出，并拒绝错误 lineage、OFD 或 epoch。 `managed/oci-capture.log`、`.build/wp10/durable-statepoint.log`、`.build/wp10/parked-block.log` | 逐阶段断连、丢 ACK、Worker/进程崩溃与源并发变化的矩阵，逐案核对原 decision、导出身份与失败 held 归宿。 |
| A07 | fb69434 冻结安装，无后代与活后代两组通过 | 两组从捕获到旧 VMM 退出均保持控制台计数和四个盘头摘要。真实 chunk 路径错误后可在 STOP 前取消并回原 Standby，STOP 后 Worker 重启只收敛退出。持久根独立恢复原 PID 和卷，临时导出 pin 释放。无后代组释放 180,129,792 字节实分配 RAM；活后代组释放 30,875,648 字节并保留 163,090,432 字节共享 RAM，后代校验 96 MiB 冷数据。相同安装件另通过真实私网 SYN 停止切点。[切点、回收口径与原始记录](hibernate-cut-qualification.md) | — |
| A08 | `fb69434` 冻结对照，修复后开发实机通过 | Raw 拒绝严格独立 Fork。Raw 和 managed Standby 均保持原 VMM 和资源图，managed 保留原 Gate。冻结件将暂停计入 Guest 时间，Fork 子机和 Hibernate 恢复时已触发定时器。修复后原机 Wake 原键重放、独立子机及新 epoch 恢复均按原 Guest 9 秒到期。[实机时钟与回执](#a08-长待机和保存点时钟) | 在包含修复的冻结安装上复跑，另验跨宿主重启。 |
| A09 | 冻结测量、开发实机，部分 | 同一真实切点恢复 RAM 和两个 writer。双盘 VMM 测试先确认两盘 flush，旧 FD 尚在时源保持 held，随后一起 pivot。 `managed/oci-capture.log`、`.build/block-kvm/live-pivot.log`、`.build/wp10/parked-block.log` | 随机截取应用多盘未完成事务的相位，覆盖合法不相等盘计数，验证跨 capture 的输入组合必拒。 |
| A10 | 开发实机，部分 | 两个 Bundle 共用 Worker 时，第二路不能取得运行中的 writer。真实 OFD 逃逸阻止旧进程通过退出围栏，三盘场景验证独立 writable heads。隔离 ext4 ENOSPC 验证 Create/Grow 未发布新 head，原写卷随后可冷启。[实际字节与故障范围](#卷恢复与停机存储实测) `.build/wp23/tracked-coexistence-kvm.log`、`.build/block-kvm/evidence.log`、`.build/wp10/product-managed-kvm.log` | 同一代际的 RW/RO 与 RW/RW、硬链接/路径别名、可共享 immutable RO 和未知 writer 的交叉矩阵。 |
| A11 | 冻结安装与开发实机，旧 checkpoint 对照通过 | 两个持久 RW 卷在保存点后各自写入新数据。原挂接释放并转挂至另一虚机，源实例删除且私有盘实际移除后，公开 `exact` 仍报告 `VOLUME_GENERATION_MISMATCH` 和 `effect=none`。`clone_conflicts` 返回两个不同的新卷 ID，恢复旧 RAM 与两盘旧字节。只读代际通过两个独立挂接共享。平台重启、克隆再休眠及精确唤醒后仍可查新卷并读到克隆写入。[实际字节、卷映射与运行范围](#卷恢复与停机存储实测) | 含新卷目录投影的冻结安装复测。 |
| A12 | fb69434 冻结公开停机卷通过，存储故障在开发实机通过 | Running 挂接、解挂与增长被拒。公开 Stop 确认原 VMM 退出，停机增长后 Guest 可独立从 16 MiB 文件系统扩到 32 MiB，解挂的持久卷可在另一实例读取。真实 ENOSPC 使 Create/Grow 无效果结束。旧盘进入只读阶段后的定点 fsync EIO 进入待恢复，Worker 重启后原操作完成，32 MiB 新盘保留原字节并可写。[开发存储故障](#卷恢复与停机存储实测) · [同包停机卷](candidate-fb69434.md#安装后公开操作) | 其他盘角色组合、在线热插拔 profile，及其他磁盘故障时序。 |
| A13 | 开发实机、Rust 测试，部分 | Native 实测 Guest /32 与 Worker 路由。FixedInternal 实测稳定 Guest 内部 IP、独立 Worker 地址与 SSH。备份恢复拒绝重叠目标网段。Worker 测试核对 route domain IPAM 及旧 attachment fence。 `.build/managed-network-native-v2.log`、`.build/managed-network-platform-fixed-v3.log`、`.build/platform-networked-backup-native-v3.log`、`.build/wp23/close-workers-tests.log` | 同内部 IP 的多个 A 实例双向通信，跨私网 ACL、hairpin、MTU、实际 guest 冒用和未受管 IPv6 的组合。 |
| A14 | 冻结测量、开发实机，部分 | 冻结 OCI 目标在激活前被终止，成功兄弟与 capture 保留。开发 TAP 在 park 和身份应用期间对 ICMP 沉默，失败身份保持隔离，父子保持独立；目标探针另检查早到 commit、错误 context 和后来一次 standby 上的旧 BEGIN。 `managed/oci-capture.log`、`.build/network-gate-libs/hostroute-kvm.log`、`smolvm/libkrun/tests/guest_gate/target_probe.py` | 各身份阶段的真实 Guest ACK 重放及断连，合并普通进程、容器、loopback 与外部 I/O 计数，验证每次坏 child 都未发布。 |
| A15 | 冻结安装、开发实机、PoC，部分 | 冻结 `d8cc5d5` 从公开 Fork Batch 创建独立子机。捕获前源机的普通 Agent、提交后两机的普通 Agent 均返回 503。服务恢复后，原 System PID 787 与嵌套 PID 1 的计数从 4 分别推进到源 554、子 527。生产 freezer 的两次 helper SIGKILL 均在 1.26–1.28 s 恢复三个计数器，回执 `failed_thawed/EPIPE`。2 vCPU PoC 另覆盖取消、超时、D 态及未应答 FUSE。[PoC 记录](checkout.md#guest-用户态冻结实验)、`.build/gate-acceptance/helper-exit-kvm.log`、`.build/gate-acceptance/installed-agent-stopped-kvm.log` | 生产路径的 D 态、FUSE 和取消边界。 |
| A16 | 冻结测量、开发实机、本地测试，部分 | 冻结 relay 经 Worker 同执行验证并执行一次授权命令，撤销后被拒。开发真实 Guest 创建与 Fork 后，克隆体和 public API 拒绝复制的 Guest bearer。 `managed/system-relay.log`、`.build/wp23/transactional-delegate-kvm.log`、`TestGuestGrantRequiresAttestedCurrentExecution` | 回滚同一源到撤销前的快照，再从新执行和实际克隆用旧 bearer 重放。 |
| A17 | 冻结测量、历史冻结、开发实机，部分 | 冻结 System Fork/Restore 通过。历史 `dbe526e` 的配套 Debian 盘证明 systemd PID 1、Docker/Compose 任务成功及 K3s 受测节点和 Pod，CoreDNS 有 Unknown。开发 System 停 Agent 时 VM 与 SSH 保持，Agent 重启后重接。 `managed/system-fork.log`、`managed/system-captures.log`、[历史候选记录](candidate-dbe526e.md)、`.build/platform-system-gateway-v3.log` | 在当前 `86dae32` 安装上重复容器矩阵，验证 K3s DNS，并做 OCI Agent 重启后原任务在同一执行继续的观测。 |
| A18 | fb69434 冻结安装，批次取消、父删除后重试和任务认领通过 | 公开批次保留原 capture 和已就绪 child。取消目标继续围栏，父机 Stop、Delete 和平台重启后，仅原失败 child 以原 ID 成功重试。公开 Task 认领返回同一 run、原 Guest PID 与原标记。首次公开提交的 HTTP 回执整体丢弃后，重启平台并以原键、原修订重试，只保留一个操作、一个 capture 和一个原子机；该子机读回原 RAM 标记。再次重放返回相同操作，改变请求返回 409。同键取消、重试和删除重放稳定。另有开发实机记录父私有写盘回收、子继续读共享闭包及父持久卷转挂后读回原字节。[首次回执丢失](candidate-fb69434.md#首次-fork-回执丢失) · [同包任务复测](candidate-fb69434.md#安装后公开操作) · [持久任务实测](managed-recovery.md#完整分叉与独立恢复后的任务索引) | — |

### A04 broker 与最后一个 RAM provider

探针从冻结安装 `fb69434` 加载 Worker 和 libkrun，走 WorkerResource 的真实 Unix socket 与 FD 传输。探针登记其加载 libkrun 的测试进程作为 VMM。活源重交三个原 backing 后退出，子机从 tick 4 继续，到 tick 9 读到原冷页的 `0xa7`。再重启 broker，存活子机重交三个 backing，孙机拿到六个 backing，从 tick 11 继续，三代 Guest PID 均为 419。

两个丢失实验各在独立 PID 命名空间运行，保留逻辑 capture 并移除落盘的 capture 目录。第一组围栏退出原源机，只有 broker 持有三个已封存 inode，再终止 broker。第二组确认唯一源 VMM 收到 SIGSTOP 后终止 broker 和源机。重启期间回收请求均得到 `E_RECOVERING`。恢复后两个 catalog 均保留原描述符摘要和 capture 引用，状态为 `lost`。GC 得到 `E_RESOURCE_PINNED`，新执行的只读租约得到 `E_RESOURCE_UNAVAILABLE`，内核返回零个 FD。夹具为 smolvm 提交 `69c4564` 的 `crates/smolvm-resourced/tests/kvm-generation.py`。

| `.build/wp23/` 实机记录 | SHA-256 |
| --- | --- |
| `a04-live-provider-fb69434-v1.log` | `b633f5fc91424918a941648f39010b96ba8963dbfdf43bb4145d3c7d538f3ff6` |
| `a04-broker-only-fb69434-v2.log` | `5da3b69f5f77dce711fccb6a54364b4dc1e36f40ab06c666c582b064e47849b2` |
| `a04-last-provider-fb69434-v2.log` | `9b3a1805864f5aeec95ec241d80da16b472c14f9c999cb30f4ca78a1c3913ec2` |

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

## 兼容、运维、备份与性能

| ID | 范围 | 已核实结果 | 待验 |
| --- | --- | --- | --- |
| A19 | 两个冻结安装，同 Worker 兼容边界通过 | `fdd620e` 和 `d8cc5d5` 分别握手证实不同 Bundle、reader 和活 ELF。交叉恢复均返回 `E_BUNDLE_UNAVAILABLE`，目标维持 epoch 0，持久根仍可用且无新分配。共享 Worker 重启后，旧 reader 恢复原 Guest PID、计数和两盘标志。开发实机另验证缺 RAM chunk 或磁盘内容改变时拒绝。[双 Bundle 安装复测](candidate-d8cc5d5.md#两个冻结-bundle-共用-worker)、`.build/wp10/durable-statepoint.log` | 历史语料拓展至 CPU/profile 变更、缺 Bundle、增量、FixedInternal、CPU/RAM 槽和活动任务。 |
| A20 | fb69434 冻结组件重启与 scope 迁移中断，broker 网络恢复待修 | 两台 Debian 的原 TCP 连接与实际卷写入跨 platformd、Serve 各自退出和重启。systemd 接收新 VMM 前后分别中断，同一公开操作恢复且只产生一份 writer。broker 再启动后重认原 writer，两条 TAP 租约停在 unknown，原流量中断。较早的跨 Worker 实测覆盖原 RAM reader 和 CPU 配额回执。[同包组件与 scope 记录](managed-recovery.md#组件重启时的网络和卷写入)、[持久 reader 记录](managed-recovery.md#运行中的-statepoint-reader-跨-worker-重启)、[CPU 配额记录](native-power.md#wp-21-运行中调整-cpu内存和-balloon) | 验证修复后 broker 重认真实 TAP 并保持原会话。Worker 主机换 boot 后分别核对 volatile 失效与 durable 恢复。 |
| A21 | 冻结测量、开发实机，部分 | 两个授权 wake demand 共用一次 exact 恢复，仅重新授权通过的请求获新执行 hold。真实 Native/FixedInternal 私网 TCP 可唤醒，授权的短 UDP 首包跨持久唤醒交付，超界数据丢弃；CLI SSH 经 Worker Gateway 可用。 `managed/lifecycle.log`、`.build/packet-udp-full-kvm.log`、`.build/platform-system-gateway-v3.log` | HTTP、TCP-mTLS、SSH 和直接 TCP/UDP 在同一候选上的并发首请求及不重放可能已送达的业务数据。 |
| A22 | 冻结安装、开发实机、本地测试，部分 | 桌面单次票据交付真实 framebuffer。Worker 确认原生运行租约后 API 才返回会话；延迟确认、丢失确认、授权撤回、到期和平台进程重启均实测。休眠后旧会话受围栏，新授权请求经现有服务唤醒策略连接原 Xvfb，Chromium noVNC 显示原画面。Guest grant 撤销立即截断进行中的 SSE。 `.build/platform-desktop-worker-lease-a30.log`、`.build/platform-desktop-worker-lease-implicit.log`、`.build/wp23/transactional-proxy-kvm.log`、`TestDesktopSessionFencesCredentialsAndExpiresLease` | 服务 revision 和证书撤销与活流进展同时发生时，分别核对新旧 HTTP、mTLS、SSH 后端以及旧 epoch。 |
| A23 | 开发实机，原生引用、并发与发布恢复通过 | Native 用同一 run 跨 Worker 和 platformd 中断备份原 checkpoint、休眠点与独立卷。并发公开删除后 Worker 引用继续保护原数据。暂存缺块和原 reader 缺失均保留已验证备份。关闭及删除旧源后在新 Worker 恢复原 RAM、根盘、独立卷和 Native `/32`。1017 在途备份升级到 1018 后保持两路原选择。六组 ext4 发布和保留中断已通过。既有 Native、FixedInternal 实测还覆盖错误密钥、等长篡改、服务 PID、隔离网络与非管理员注册拒绝。[原生备份与双路记录](#a23-原生备份选择与中断恢复)、[发布中断记录](#a23-备份发布与保留中断)、`.build/platform-networked-backup-native-v3.log`、`.build/platform-networked-backup-fixed-v3.log` | 在下一冻结包分别复跑 Native 与 FixedInternal 的交接中断。 |
| A24 | 历史 0.1.6 到 schema 1017、1018、1019，迁移中断与回退通过 | 三个候选迁移中被 SIGKILL 后，19 张旧表及全部行一致。旧二进制拒绝打开新 schema。新旧发行件分别用匹配的备份恢复，原 owner 可登录，旧 control epoch 的公开写入返回 409。另在隔离的 Ubuntu 24.04 KVM 整机上通过安装器分别从 0.1.6 升到 1018 和 1019，整机断电重启后 systemd、owner 和两个版本的备份再次验证。[发行件与整机记录](#a24-历史发行件升级与回退)、[事务夹具](../../internal/registry/deep_migration_test.go) | 最终发行锁确定后按该版本复跑。 |
| A25 | 开发实机，部分 | 两路 Serve 共用实际 cgroup slice 和 Worker 预算，六路并发启动被拒，受保护 VM 保持。混合 Bundle 另核对两个真实 VMM scope 与共用 writer/IPAM。 `.build/worker-pressure-kvm-v5.log`、`.build/wp23/tracked-coexistence-kvm.log` | 反向启动顺序、并发 Fork/Restore 风暴和各 Bundle 的共享/私有内存统计。 |
| A26 | 冻结安装，官方与定制实机通过 | Chromium 创建默认 Native 的停机 System，并禁用状态 `unknown` 的 Fork batch。TS 挂卷后冷启动，CLI Standby/Wake 保持同一执行，TS 从完整 VM checkpoint 恢复原 RAM 标记与 `/dev/vdb` 字节，Python 批量创建两个独立 writer 和新 Native 地址。默认变更只影响新建机器，CLI 接受 Fixed 覆盖。官方 v1.37.1 的真实 OCI 保持 Pause/Resume 与 `managed_clean` clone 语义，实例间写入彼此独立。浏览器分开显示电源、Guest 管理、工作负载、网络与服务状态，并保留官方兼容动作。Python Standby 得到 `CAPABILITY_UNSUPPORTED`。 `.build/platform-a26-d8cc-installed-v2.log`、`.build/a26-d8cc-evidence/` | — |
| A27 | 开发实机，部分 | 本地 ext4/KVM 对照了冷启动、双分支和三层递归，有界 dirty、首次/重复 durable restore 与 16/4/0 读通道。记录 p50/p95、首任务、源心跳空档、PSS、双盘正确性，默认保持 16 条通道。 [WP-22 测量与原始样本路径](performance-wp22.md) | 精确 vCPU 停顿、dirty 集合闭包、rebase、预取/共享缓存的独立对照，以及网络存储和更高并发。 |

### A23 备份发布与保留中断

公开 API 提交第二次备份时，第一份已用发行版 sandboxctl 校验。夹具在私有 ext4 备份目录订阅 Linux fanotify 打开事件。它用 pidfd 在指定文件操作途中终止 platformd，并通过正常启动入口重启。每日策略保留一份归档。旧平台 c011476 在目录改名后留下暂存标记，在新回执已提交后中断则要等下一个备份周期才会继续回收旧目录。

当前平台在公布已验证回执前同步父目录，重启时重新验证已发布的归档。备份保留流程在启动时核查待处理的回执，并在删除上一份归档前确认保留份数有足够的可验证副本。六组复测覆盖暂存、已改名但回执未提交、已验证但尚未开始清理、旧目录进入清理、旧目录已删除但回执尚未更新，以及重启时最新归档遭到单字节篡改。每次恢复均保留同一运行 ID、暂存数据库 inode 和内容摘要。损坏场景先独立验证上一份仍可恢复，修复最新副本再完成保留处理。六组 ext4 实测日志 `.build/wp23/a23-publication-ext4.log` 的 SHA-256 为 `f21203617b6c7d3bd0cc7068ef4f2c538c8e43882bfb8e4ecc0ad01ec1ab5bf7`。[备份中断夹具](../../tests/integration/backup_publication_crash.py)

### A23 原生备份选择与中断恢复

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

### A26 同平台的双运行时

A26 由同一个 `platformd` 进程连接冻结 `d8cc5d5` 的私有 Worker Serve 和官方 Serve，两套运行时各用独立状态目录。冻结安装包摘要为 `sha256:c5070578cdf0d4fd01cc120721515c169e9297a3f624803def398996c0e249b5`，平台、浏览器资源和客户端基于锁定的 `9eb18ae`。官方归档 SHA-256 为 `f8d5d1aaa198c7b9e676fa4365998e22fc9051a2199b208741be5f38fef5e404`，解包的九个文件逐一与 `packaging/runtime-manifest.json` 匹配。官方 clone 使用发行版状态 helper 的 `SO_PEERCRED` 与固定名称校验读取实际 CLI 的 `restart_policy`、绑定标签与状态，夹具将 helper 的 UID 和路径设为本次隔离实例。平台状态使用独立的 4 GiB 临时文件系统，运行盘置于外部 ext4，磁盘准入沿用发布默认值。复测日志 SHA-256 为 `98333564ae728ad43a85e89ee74a02a728519d260d6544f738d9c88ba3441cfe`。

## 自动生命周期

| ID | 范围 | 已核实结果 | 待验 |
| --- | --- | --- | --- |
| A28 | `86dae32` 冻结安装和更新 Worker，同机实测 | 公开策略等待 Worker 回执时显示 `pending`。同一 Debian 逻辑实例分别核对 Standby 单开、Hibernate 单开、共用 idle 起点的两阶段、停驻证据失联后的阻塞、raw 停驻 profile 的阻塞和旧 `idle_policy` 仅休眠。Hibernate 恢复的新执行和同一执行的两次短 Standby Wake 都获得三个新 collector 会话。迟到报告被围栏，两路公开详情在 Worker 失联时一致显示 blocked。另已核对全局默认、覆盖与批量应用。 `managed/lifecycle.log`、`.build/wp10/debian-lease-and-protected-fork-kvm-v2.log`、`.build/a28-idle-policy-kvm-v6.log`、`.build/wake-collector-kvm-v2.log` | — |
| A29 | 冻结安装、开发实机，代表场景通过 | Guest 等待 TLS SSE 超过 idle 阈值时仅有 `hold:credential_proxy`，流结束后同一执行自动 Standby。私网 Guest 直连 HTTP SSE 时保持源出站和目标入站证据。Gateway OpenSSH 静默 PTY 有 `hold:service_access` 和真实连接证据，独立 Guest Relay 前台 Exec 静默 12 秒仅有 `hold:guest_exec`。等待客户端的桌面会话仅有 `lease:desktop_session`。新版 System Agent 用公共终端 API 验证权限、断连时的 Worker hold、按偏移重接及退出码。各项活动结束后，仍有监听 daemon 的同一执行实际进入 Standby。 `.build/wp23/transactional-proxy-kvm.log`、`.build/guest-direct-sse-kvm.log`、`.build/platform-desktop-worker-lease-a30.log`、`.build/platform-guest-silent-exec-a29-v2.log` | 在下一完整冻结安装中复测含 `system-exec-stream-v1` 的 System 镜像。 |
| A30 | 冻结安装、开发实机，代表竞态通过 | 排队中的压力休眠分别被同一 Worker 新确认的 Access hold 和 `keep_running` 取消。公共 `keep_running` 在空闲 Standby 排队期间确认，Worker 记录取消且未提交原生操作。另一次已提交的 Standby 完成后，新桌面 Access 在原 epoch 唤醒并取得正式租约。获确认的 v2 续约是原到期时间之后唯一 blocker，释放后进入 Standby。两个并发公共请求与恢复 monitor 推进同一运行租约，连续两轮均只产生一个有效租约。 `.build/worker-pressure-kvm-v9.log`、`.build/wp23/lease-serial-installed-kvm-1.log`、`.build/wp23/lease-serial-installed-kvm-2.log`、`.build/platform-desktop-worker-lease-a30.log` | 补续租与 `keep_running` 各自在已提交候选的停止切点前后并发，以及各入口交叉组合。 |
| A31 | d8cc5d5 冻结安装，八组私网停止切点通过 | Native、FixedInternal 各从 Running 和 Standby 在 `STOP_COMMIT` 前后复测。提交前获准的私网 SYN 撤销休眠并保留原 epoch，原 Standby 随后唤醒。提交后两个 SYN 共用一次原 StatePoint 精确恢复，epoch 增加一次。两个方向另已验证有界 UDP 交付与大包丢弃，fb69434 冻结测量再次通过。[fb69434 同包测量](candidate-fb69434.md#九项包内测量) · [d8cc5d5 安装复测](candidate-d8cc5d5.md) · [fdd620e 安装复测](candidate-fdd620e.md) | 服务和 API 入口在停止切点的撤权及新执行 hold。 |
| A32 | 冻结测量、开发实机，部分 | 冻结 Worker 将过期 v1 保持 suspect，v2 标 expired，未知 activity 仍阻止睡眠。开发公共续租与 stale revision 冲突通过，受保护父子保有独立 leases。 `managed/lifecycle.log`、`.build/wp10/debian-lease-and-protected-fork-kvm-v2.log` | 子 VM 重放父 lease token 和旧 epoch token 的公网请求，及 UI 对 suspect、expired、unknown 的实机解释。 |
| A33 | 同 Bundle 和双 Bundle 冻结安装，压力与共享引用通过 | 两路 Serve 共用单个 Worker 账本和隔离 slice。六个并发启动、Fork、Restore 在候选全受保护时被拒绝，两个 VMM 继续。两套冻结 Bundle 并存时仅低优先级 Guest 休眠，实收 62,935,040 字节。同 Bundle 组实测六个共同 RAM inode 在第一子机退出后保留，第二子机退出后 Worker 关闭托管 FD，活父机继续持有。父机完成休眠后 `shmem` 为 0，`memory.current` 总降 111,386,624 字节。[d8cc5d5 安装复测](candidate-d8cc5d5.md) | 在含 balloon 反馈的冻结发行包核对自动调节与未响应时的预算。 |
| A34 | d8cc5d5 冻结安装，重启与唤醒恢复通过 | platformd 强制退出后仅释放该进程的连接，独立运行租约保留，迟到的旧申请被拒。Worker 经 SIGKILL 后从同一安装件重启，原 VMM 和已确认策略继续，旧 PTY hold 进入 `suspect`，重放的旧活动序号无法解除 `unknown`。丢失应答的唤醒与另一请求合并为一次精确恢复，重授权只给获准请求授予新执行 hold，旧 epoch 无法续租。持续压力下 20 秒唤醒宽限得到验证。[d8cc5d5 安装复测](candidate-d8cc5d5.md)、[fdd620e 安装复测](candidate-fdd620e.md) | — |
| A35 | 开发实机，部分 | 受保护的 System 源在 `auto_hibernate=false`、`keep_running=true`、有效 lease 和活动任务等待时主动 Fork；两子原任务保留 PID 与 nonce，继承策略，独立 lease 与回复。 `.build/wp10/debian-lease-and-protected-fork-kvm-v2.log` | 保持真实模型 SSE 活跃时同一源主动 Fork/Batch，并校验子会话的授权及挂起 I/O 边界。 |
| A36 | fb69434 冻结安装通过 Stop、首桌面和禁止失败冷启，开发 Worker 的显式 exact 重试通过 | API 隐式唤醒策略分别约束 Running 与 Hibernated，私网 TCP 与已有桌面分别按其服务策略唤醒。冻结 Stop 在普通 Agent 停用时确认 writer、旧执行和待处理唤醒受到围栏，重新冷启读回根盘、挂载卷和裸盘。冻结首桌面从 epoch 0 空槽并发启动一个执行，复用租约并回收确认期间撤销的授权，真实传输 800×480 画面。两种保存恢复故障都保留同一 Hibernated 保存点，冻结 Worker 拒绝冷启。一个已发给失败目标的 writer grant 完整释放后，此版的显式 exact 仍返回 `VOLUME_GENERATION_MISMATCH`。修复后的 Worker 在同一测试中确认失败目标退出、授权释放且尚未发出身份 BEGIN，随后从原保存点恢复 RAM。[开发修复与同夹具对照](candidate-fb69434.md#保存唤醒修复的开发复测)。[同包 Stop、首桌面与失败原记录](candidate-fb69434.md#安装后公开操作) · `.build/wp10/debian-lease-and-protected-fork-kvm-v2.log` · `.build/platform-desktop-worker-lease-wake-policy-v3.log` | 用修复后的冻结运行时验证围栏失败目标后的 exact 重试，确认原保存点、卷和 RAM。 |

当前正式发行门采用 [验收定义](ACCEPTANCE_AND_OPERATIONS.md#10-发行完成清单) 的能力声明规则。上表的待验项仍需要对应的实验或明确的发布范围决定。

同机 A28 测试以只读挂载使用 `86dae32` 安装，Bundle 摘要 `sha256:24e874e4bdddbb18c3bfb93e4562e720953bac6ae77626618276a83f0fd277d1`。同一 Debian 根盘 SHA-256 为 `065e82d9053c8a4a1ed6ba9030ccd816b3791f498576648a9a11010ee33b281d`，平台和 Worker catalog 使用独立临时文件系统。两阶段分别使用 4 秒及 30 秒阈值，来自同一个观测的单调 idle 起点。Guest 报告过期后仍有确认的 managed hold，Worker 完成休眠。另一次待机完成后暂时移开该执行的原生控制 socket，平台显示 `blocked` 与 `unknown:native_inventory`。恢复 socket 后，原执行继续停驻且仍因 `unknown:guest` 被阻塞，直到显式 Wake。真实 raw 停驻通过 `unknown:standby_hold` 保守阻止自动休眠。旧请求将已启用的压力回收改为 `disabled` 并仅安排 Hibernate。各次显式恢复核对了原 RAM 标记。 [复测脚本](../../tests/integration/lifecycle_idle_kvm.py) `.build/a28-idle-policy-kvm-v6.log`（SHA-256 `17f2d425f880ff4578cea29ca9767b617f3c3114d543f088ccd4f48c1ee563ce`）。

短暂停驻的追加实机使用相同安装和 Debian 根盘，在独立挂载命名空间将 Worker `7e9e3a7` 与重新测量的 manifest 覆盖到只读安装路径，得到开发摘要 `sha256:27b857359bd8358f0051cee25061d5d0f98c7fd80a0a5704799ba49654aaa99a`。两次 Wake 均在 Guest 报告 TTL 过期前返回同一执行，Worker 清掉三份旧报告。复测分别拒绝了旧会话、未登记会话的旧代际报告和带新代际的旧会话，随后平台以三个新会话取得确认。Worker 停止响应期间，机器详情及 policy GET 均显示 `blocked`、`unknown:worker`，恢复实测后均显示 `applied`。同一 fixture 从 1017 平台保留已入队但尚未确认的旧请求，升级到 1019 后由 Worker 确认禁用压力回收，原操作正常完成。1019 只纠正未指定压力回收的 legacy 设置。历史 Worker 缺少代际字段时继续使用其原有私有报告合同。 [复测脚本](../../tests/integration/lifecycle_wake_collector_kvm.py) `.build/wake-collector-kvm-v2.log`（SHA-256 `be298cf628b750e159039a8846b0e26fd4ad8b78b3aecb5973156e03d53faadc`）。

公共可重连 Exec 实测使用冻结 `d8cc5d5` 安装的 Worker 和 VMM，Bundle 摘要为 `sha256:c5070578cdf0d4fd01cc120721515c169e9297a3f624803def398996c0e249b5`。正例在独立不可变 Debian 根盘中安装 `f027f2d` 构建的 System Agent，根盘 SHA-256 为 `131c58f2786ae5e01cb76f646879dbb6dc708d51a5c870ceb2ae1ee9bbe59360`，Agent SHA-256 为 `b8d5ecbc74dcc03d147f612849e651c1dad3ec9c6c659107863b2551caa812cf`。机器范围的管理令牌创建了 PTY。连接断开并静默 5 秒后，Worker 仍报告 `hold:api_access`、`guest:exec`、`platform:exec` 和 `platform:stream`。新单次票从原输出偏移继续，stdin 重放只生效一次，窗口变为 31×101，退出码为 17。非 TTY 回传分离的 stdout 和 stderr 及退出码 9。终端读者凭据撤销后，其连接关闭。平台进程被强制终止后，Guest 回收了对应命令 PID，旧会话进入 `session_lost`，旧输入被拒，只有监听 daemon 的原执行再次进入 Standby。保留原冻结 Debian 镜像的反例在发送命令前返回 `NOT_SUPPORTED`，Worker 释放了访问保护。 [复测脚本](../../tests/integration/native_exec_kvm.py) `.build/platform-native-exec-reconnect-kvm-v4.log`（SHA-256 `37951fcde444bd9c937c18dfd249cc2a7e72d7d9edf6dbed495f6eb5fda41a3f`）、`.build/platform-native-exec-legacy-kvm-v2.log`（SHA-256 `f223b3c8d3086328c9daae87d58166e9f672d1bc3e15a21f173f8ba0ede86e82`）。

同一平台构建回归官方 Serve 1.37.1。OCI 命令默认在 `/workspace` 运行，同键重放复用原 Exec。新票从输出标记之后续读并取得退出码 13。 `.build/platform-stock-exec-compat-kvm.log`（SHA-256 `cf0aa3ebc4c25c39b27bfd968d53e6b0eab74764fe16bf08dbdf9093f9c3e55d`）。

桌面租约实测使用冻结 `fdd620e` Worker 和平台 `b0de58d`。夹具先暂缓送达桌面租约申请，已确认的 `hold:service_access` 在申请期间保护 Running，平台尚未发布会话。丢失成功答复后，相同幂等键得到同一个确认租约，Worker 只记录一次申请。答复送达前撤销凭据，平台拒绝发布并释放该租约。平台进程被强制终止后，独立 Worker 租约继续生效，新票据可连接原 Xvfb 进程。另实际从 schema 1016 升至 1017，历史会话保留原 ID、到期时间和执行身份，取得同 ID 的 Worker v2 租约后才发新票。 `.build/platform-desktop-worker-lease-a30.log`、`.build/platform-desktop-worker-lease-upgrade-v2.log`
