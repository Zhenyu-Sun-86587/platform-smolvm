# WP-24 发布验收台账

2026-11-26 根据已有记录核验 [A01–A27](ACCEPTANCE_AND_OPERATIONS.md#2-核心能力完成矩阵) 与 [A28–A36](ACCEPTANCE_AND_OPERATIONS.md#122-新增必要验收)。每行记录观测到的结果和余下的组合验收。测试在隔离的 Linux x86_64 Worker 或本地测试环境进行。

| 证据名称 | 构建与运行范围 |
| --- | --- |
| 冻结安装 | 已安装的 smolvm `91e2c72`，Bundle `sha256:512366a367c3f424d20c787b2144d81b1719128468212b69ef4db078e1821ffc`。安装路径为 `/var/tmp/deepfork-smolvm-build-cache/release-91e2c72/installed`。安装后用分发 wrapper 启动了两个 KVM OCI Guest，重测的 OCI 证据与打包前逐字节相同。另从安装目录启动了 Serve 并核对 reader 和 Bundle 摘要。握手使用无 Worker 配置，Worker 依赖的能力返回 `WORKER_NOT_CONFIGURED`。本地记录为 `.build/release-91e2c72/install.log`、`.build/release-91e2c72/installed-smoke.log` 和同安装目录旁的 `installed-handshake.json`。 |
| 冻结测量 | 同一冻结构建锁和产物打包前的五组真机测量。下文的 `managed/*.log` 位于该安装目录的 `metadata` 下，五份日志的 SHA-256 均与 `managed-acceptance.json` 相符。测量元数据记录平台验收代码 `b2b387a` 和 smolvm 验收代码 `9eba0db`，与 Bundle 的源码修订分别登记。生命周期实验通过 Worker IPC 注入受控制的活动报告，A29 的开发真机另使用实际收集器。 |
| 开发实机 | `.build/` 下以工作区为根的具名 KVM 记录。各实验使用当时的开发 Bundle 或 stock runtime。 |
| 历史与本地测试 | 旧冻结候选单独标明版本。Go、Rust、Python 的契约或注入测试保留其覆盖范围。 |

## 捕获、存储、网络与 Guest

| ID | 范围 | 已核实结果 | 待验 |
| --- | --- | --- | --- |
| A01 | 冻结测量、开发实机，部分 | 一次 OCI held cut 恢复两个子 VM，两者处于相同 RAM 和两个 writer 的边界。开发批次检查固定 child ID 和单一 capture，性能样本记录 RAM backing 与宿主 I/O。 `managed/oci-capture.log`、`.build/platform-native-oci-fork.log`、[WP-22 样本](performance-wp22.md) | 同一次冻结安装运行中对齐 capture ID、每个子 backing 引用和文件系统写入/全拷贝追踪。 |
| A02 | 冻结测量、开发实机，部分 | 两级祖先和兄弟均已围栏后，孙 VM 继续原进程与两块盘。独立的 KVM 探针读回未预触碰冷页和分支私有页，记录 sealed memfd 与私有映射。 `managed/oci-capture.log`、`.build/resource-kvm/evidence.log` | 合并验证多代盘冷读、打开 fd 的 offset，及祖先目录、socket 实际移除。 |
| A03 | 冻结测量、开发实机，部分 | 冻结 OCI 递归与重复恢复通过，同机恢复原 PID 和准确下一 tick。性能基线走到三层，持续校验两个独立写盘。 `managed/oci-capture.log`、[递归与持久恢复记录](performance-wp22.md) | 同棵递归树分别删除中间和根祖先，同时写各分支，再独立 Hibernate/Restore 子分支，核对逻辑谱系与末引用回收。 |
| A04 | 开发实机、Rust 测试，部分 | broker 重启后由仍活着的原 child VMM 提供 sealed RAM 原 inode 和两块可写头，随后公开 Fork 继续原 PID。Rust 测试确认宿主启动身份变化时 volatile 被标为 lost，意图保留。 `.build/wp23/tracked-provider-kvm.log`、`.build/wp23/close-workers-tests.log` 中 `catalog::tests::boot_change_marks_volatile_resources_lost_and_preserves_intents` | 独立真机矩阵覆盖仅 broker 持有、唯一 provider 同时退出且没有 durable 根，以及各情形的 GC/错误回执。 |
| A05 | 开发实机、本地测试，部分 | 实时备份观察到 portable snapshot 与真实 Worker StatePoint/block roots 同时持有 pin。旧 reader 仍有运行或持久引用时受到保留保护。 `.build/platform-networked-backup-native-v3.log`、`.build/wp23/tracked-coexistence-kvm.log`、`smolvm/crates/smolvm-resourced/src/catalog_tests.rs` | 在一个真实对象图上并发结束最后引用、未决操作、两类导出 pin 与休眠根，并于 tombstone 后重启回收。 |
| A06 | 冻结测量、开发实机，部分 | 冻结 OCI 在 child 激活前失败，重试保留已成功 child 和原 capture。开发 Worker 记录 STOP_COMMIT 重启后的原执行退出，并拒绝错误 lineage、OFD 或 epoch。 `managed/oci-capture.log`、`.build/wp10/durable-statepoint.log`、`.build/wp10/parked-block.log` | 逐阶段断连、丢 ACK、Worker/进程崩溃与源并发变化的矩阵，逐案核对原 decision、导出身份与失败 held 归宿。 |
| A07 | 冻结测量、开发实机，部分 | 冻结 OCI 从停驻点到旧 VMM 退出期间计数未推进。exact 恢复保留原 PID、紧接下一 tick 和原两个卷。开发实机核对 STOP_COMMIT 重启围栏、缺 chunk 及改盘拒绝。 `managed/oci-capture.log`、`.build/wp10/durable-statepoint.log` | 有活后代与无后代两组比较，关联外部 I/O、STOP 前回原态和 STOP 后仅退出，核对冗余 pin 释放、后代冷页及实际节省。 |
| A08 | 冻结测量、开发实机，部分 | 真实控制器从 Running 进入 Standby，越过 guest 报告 TTL 后继续 Hibernate，后续按原执行点恢复。开发 managed Standby/Wake 以同一 execution epoch 得到 STANDBY 和 RUNNING 回执。 `managed/lifecycle.log`、`.build/network-gate-libs/platform-native-power-combined.log`、`smolvm/libkrun/tests/guest_gate/probe.py` | 当前候选的 raw 拒绝 strict clone、长待机原点、无新增 RAM 持久化及盘代际、时钟仅补偿一次的联合测量。 |
| A09 | 冻结测量、开发实机，部分 | 同一真实切点恢复 RAM 和两个 writer。双盘 VMM 测试先确认两盘 flush，旧 FD 尚在时源保持 held，随后一起 pivot。 `managed/oci-capture.log`、`.build/block-kvm/live-pivot.log`、`.build/wp10/parked-block.log` | 随机截取应用多盘未完成事务的相位，覆盖合法不相等盘计数，验证跨 capture 的输入组合必拒。 |
| A10 | 开发实机，部分 | 两个 Bundle 在同 Worker 竞争，第二个被拒绝运行中的 writer。真实 OFD 逃逸时对旧进程的 death fence 保持阻塞，三盘场景验证独立 writable heads。 `.build/wp23/tracked-coexistence-kvm.log`、`.build/block-kvm/evidence.log`、`.build/wp10/product-managed-kvm.log` | 同一代际的 RW/RO 与 RW/RW、硬链接/路径别名、可共享 immutable RO 和未知 writer 的交叉矩阵。 |
| A11 | 冻结测量、开发实机，部分 | exact Hibernate 恢复原卷，且另一次逻辑恢复得到独立 writable heads。篡改保存点所需盘内容或移除 RAM chunk 时 durable reader 拒绝。 `managed/oci-capture.log`、`.build/wp10/statepoint-platform-oci-v2.log`、`.build/wp10/durable-statepoint.log` | 确认原卷已独立推进后，以同一旧 checkpoint 对照 exact 冲突与 `clone_conflicts` 的新卷 ID、内容和映射。 |
| A12 | 开发实机，部分 | 公共产品入口实际创建、快照、克隆、扩容，System KVM 启动装载预约的 RW 与 RO 头。真实 Worker 拒绝错误 consumer、插槽及过期 epoch。 `.build/wp10/product-platform-kvm.log`、`.build/wp10/product-managed-kvm.log` | 所有盘角色的统一设备清单、在线 attach/detach，及部分扩容、ENOSPC、fsync 故障后的最终数据和回执。 |
| A13 | 开发实机、Rust 测试，部分 | Native 实测 Guest /32 与 Worker 路由。FixedInternal 实测稳定 Guest 内部 IP、独立 Worker 地址与 SSH。备份恢复拒绝重叠目标网段。Worker 测试核对 route domain IPAM 及旧 attachment fence。 `.build/managed-network-native-v2.log`、`.build/managed-network-platform-fixed-v3.log`、`.build/platform-networked-backup-native-v3.log`、`.build/wp23/close-workers-tests.log` | 同内部 IP 的多个 A 实例双向通信，跨私网 ACL、hairpin、MTU、实际 guest 冒用和未受管 IPv6 的组合。 |
| A14 | 冻结测量、开发实机，部分 | 冻结 OCI 目标在激活前被终止，成功兄弟与 capture 保留。开发 TAP 在 park 和身份应用期间对 ICMP 沉默，失败身份保持隔离，父子保持独立；目标探针另检查早到 commit、错误 context 和后来一次 standby 上的旧 BEGIN。 `managed/oci-capture.log`、`.build/network-gate-libs/hostroute-kvm.log`、`smolvm/libkrun/tests/guest_gate/target_probe.py` | 各身份阶段的真实 Guest ACK 重放及断连，合并普通进程、容器、loopback 与外部 I/O 计数，验证每次坏 child 都未发布。 |
| A15 | PoC、开发实机，部分 | 2 vCPU PoC 测了 SIGKILL、取消、超时、D 态与未应答 FUSE，全部确认三个计数器继续。另在生产 freezer 的两次 KVM 实验中，测试 helper 经真实测量注册后 SIGKILL，约 21 ms 武装，1.26–1.28 s 后三个计数器恢复，回执为 `failed_thawed/EPIPE`。 [PoC 记录](checkout.md#guest-用户态冻结实验)、`.build/gate-acceptance/helper-exit-kvm.log` | 生产路径的 D 态、FUSE 和取消边界。实际先停正在运行的普通 Agent 再完成同一公开 capture/fork 的夹具在 shell 计数器初始化处退出，尚无成功结果。 `.build/gate-acceptance/agent-stopped-kvm.log` |
| A16 | 冻结测量、开发实机、本地测试，部分 | 冻结 relay 经 Worker 同执行验证并执行一次授权命令，撤销后被拒。开发真实 Guest 创建与 Fork 后，克隆体和 public API 拒绝复制的 Guest bearer。 `managed/system-relay.log`、`.build/wp23/transactional-delegate-kvm.log`、`TestGuestGrantRequiresAttestedCurrentExecution` | 回滚同一源到撤销前的快照，再从新执行和实际克隆用旧 bearer 重放。 |
| A17 | 冻结测量、历史冻结、开发实机，部分 | 冻结 System Fork/Restore 通过。历史 `dbe526e` 的配套 Debian 盘证明 systemd PID 1、Docker/Compose 任务成功及 K3s 受测节点和 Pod，CoreDNS 有 Unknown。开发 System 停 Agent 时 VM 与 SSH 保持，Agent 重启后重接。 `managed/system-fork.log`、`managed/system-captures.log`、[历史候选记录](candidate-dbe526e.md)、`.build/platform-system-gateway-v3.log` | 在同一 `91e2c72` 安装上重复容器矩阵，验证 K3s DNS，并做 OCI Agent 重启后原任务在同一执行继续的观测。 |
| A18 | 冻结测量、开发实机，部分 | 冻结失败 child 重试复用 capture、保持兄弟，仅失败 child 的 epoch 增加。开发公开重试跨 HTTP 执行丢失与平台重启，`best_effort` 保持已准入 ID 与拒绝 ID；活动任务子进程保持同 PID 和 nonce。 `managed/oci-capture.log`、`.build/platform-oci-fork-retry-kvm.log`、`.build/wp23/tracked-provider-kvm.log`、`.build/wp10/debian-lease-and-protected-fork-kvm-v2.log` | 把部分失败、重复同 key、取消及父删除后的 Task adoption 放进同一批次，检查 task API 不再查询活父。 |

## 兼容、运维、备份与性能

| ID | 范围 | 已核实结果 | 待验 |
| --- | --- | --- | --- |
| A19 | 冻结安装、开发实机、本地测试，部分 | 冻结安装从自身 Serve 握手取得固定 reader。同 Worker 的另一个 Bundle 拒绝不匹配的持久点，原 reader 在共享 Worker 重启后成功恢复；missing chunk 或改变的盘字节被拒。 `installed-handshake.json`、`.build/wp23/tracked-coexistence-kvm.log`、`.build/wp10/durable-statepoint.log` | 保留并复跑正式历史语料，覆盖 CPU/profile 变更、缺 Bundle、增量、FixedInternal、CPU/RAM 槽和活动任务等输入。 |
| A20 | 开发实机，部分 | 平台重启保留逻辑绑定与操作回执。私有 Serve 重启保持 VMM。单独重启承载 broker 的 resourced 后检测原 PID、FD、OFD，可用 provider 自动 re-export。旧 Bundle Serve 可从原 state 重新启动。 `.build/wp10/product-managed-kvm.log`、`.build/wp23/tracked-provider-kvm.log`、`.build/wp23/tracked-coexistence-kvm.log` | 在同一安装批次分开记录每个组件的重启、scope 迁移中断、运行中的网络与 writer。宿主断电耐久性仍待独立场景。 |
| A21 | 冻结测量、开发实机，部分 | 两个授权 wake demand 共用一次 exact 恢复，仅重新授权通过的请求获新执行 hold。真实 Native/FixedInternal 私网 TCP 可唤醒，授权的短 UDP 首包跨持久唤醒交付，超界数据丢弃；CLI SSH 经 Worker Gateway 可用。 `managed/lifecycle.log`、`.build/packet-udp-full-kvm.log`、`.build/platform-system-gateway-v3.log` | HTTP、TCP-mTLS、SSH 和直接 TCP/UDP 在同一候选上的并发首请求及不重放可能已送达的业务数据。 |
| A22 | 开发实机、本地测试，部分 | 桌面单次票据交付真实 framebuffer，租约到期释放；休眠后旧会话受围栏，新票据访问原 Xvfb。Guest grant 撤销立即截断进行中的 SSE。 `.build/platform-desktop-hibernate-kvm-final.log`、`.build/wp23/transactional-proxy-kvm.log`、`TestDesktopSessionFencesCredentialsAndExpiresLease` | 服务 revision 和证书撤销与活流进展同时发生时，分别核对新旧 HTTP、mTLS、SSH 后端以及旧 epoch。 |
| A23 | 开发实机，部分 | Native 与 FixedInternal 的真实 StatePoint 备份均在暂存时持有实际根 pin。错误恢复密钥、缺失块和等长篡改被拒，last-good 保留。隔离的物理 Worker 导入完整 closure，保留 tmpfs 标记与原 Guest 服务 PID，原源不动。 `.build/platform-networked-backup-native-v3.log`、`.build/platform-networked-backup-fixed-v3.log` | 并发 GC 及备份提交各中断点的组合，缺匹配 Bundle、旧 reader 保留与原 last-good 目录的断点复跑。 |
| A24 | SQLite/旧二进制，代表夹具通过 | 用历史 schema 2 SQL 构造实际 SQLite 文件，迁移保留 pause 证据和 control epoch；审计选定 owner，歧义时保留旧 scoped token 并拒绝误选 owner。事务中注入冲突，schema、原行和先前 DDL 均回滚。实际旧 platformd 拒绝 schema 1014。 [迁移夹具](../../internal/registry/deep_migration_test.go)中的 `TestDeepMigrationPreservesPauseEvidence`、`TestDeepMigrationKeepsAmbiguousLegacyOwnerUnassigned`、`.build/wp23/interrupted-migration.log`、`.build/wp23/old-platform-guard.log` | 归档此次平台发行包的最终升级/回退实机记录，并补进程强制中断与备份恢复。 |
| A25 | 开发实机，部分 | 两路 Serve 共用实际 cgroup slice 和 Worker 预算，六路并发启动被拒，受保护 VM 保持。混合 Bundle 另核对两个真实 VMM scope 与共用 writer/IPAM。 `.build/worker-pressure-kvm-v5.log`、`.build/wp23/tracked-coexistence-kvm.log` | 反向启动顺序、并发 Fork/Restore 风暴和各 Bundle 的共享/私有内存统计。 |
| A26 | 冻结安装、开发实机、本地测试，部分 | 冻结 wrapper 两次 OCI 启动和 Serve 能力声明通过。stock 1.37.1 实机适配器测试通过，覆盖启动、exec、pause ID、resume、stop 和清理；Go 契约另覆盖深路线能力拒绝。UI 构建和 MCP 权限/幂等验收有独立记录。 `.build/release-91e2c72/installed-smoke.log`、`.build/a26-stock-runtime-final.log`、`.build/platform-a26-ui-build.log`、`.build/platform-mcp-proof.log`、`TestFullStateAdmissionPreservesLegacyAndRefusesBeforeWrite` | 补 stock 旧 clone 的实机终态，并在同一安装上用 UI、TS、Python、CLI 核对四态、健康分离、unknown 能力和窄回归。 |
| A27 | 开发实机，部分 | 本地 ext4/KVM 对照了冷启动、双分支和三层递归，有界 dirty、首次/重复 durable restore 与 16/4/0 读通道。记录 p50/p95、首任务、源心跳空档、PSS、双盘正确性，默认保持 16 条通道。 [WP-22 测量与原始样本路径](performance-wp22.md) | 精确 vCPU 停顿、dirty 集合闭包、rebase、预取/共享缓存的独立对照，以及网络存储和更高并发。 |

## 自动生命周期

| ID | 范围 | 已核实结果 | 待验 |
| --- | --- | --- | --- |
| A28 | 冻结测量、开发实机，部分 | 真实 Worker 从 Running 经 Standby 到 Hibernate，受控冻结持续超出 Guest 报告 TTL。开发 API 独立核对默认值、覆盖、批量应用和 applied revision。 `managed/lifecycle.log`、`.build/wp10/debian-lease-and-protected-fork-kvm-v2.log` | Standby 与 Hibernate 各自单开、hold 证据丢失和未知 profile 的实机组合，确认同一 idle 起点和历史仅 Hibernate 语义。 |
| A29 | 冻结测量、开发实机，部分 | 冻结 Worker 未知活动和受保护的 SSH hold 会阻止睡眠。开发合版真实 Guest 等待 TLS SSE 时，Worker 唯一 blocker 为 `hold:credential_proxy`，超过 2 s idle 阈值仍运行。正常和撤销结束均释放保护，随后同一执行进入自动 Standby。 `managed/lifecycle.log`、`.build/wp23/transactional-proxy-kvm.log` | 补实际低 CPU SSH/PTY/Exec 与仅监听 daemon 的并列负载，独立验证各入口的活动归因。 |
| A30 | 开发实机、本地测试，部分 | 新 platformd 按归属回收旧连接，独立租约保持，迟到 Worker admission 保持关闭。公共 v2 续租和旧 revision 冲突按 receipts 重验。 `.build/wp23/platform-connection-recovery-kvm-v2.log`、`.build/wp10/debian-lease-and-protected-fork-kvm-v2.log` | 在自动候选已排队与已 dispatch 两侧分别竞态变更 keep_running、续租与新 Access，核对已 ACK 保护的生效顺序。 |
| A31 | 冻结测量、开发实机，部分 | 两次离线唤醒需求共享唯一 exact restore，撤销中的请求未被授权到新执行。开发私网 TCP SYN 和有界 UDP 分别验证了 durable wake。 `managed/lifecycle.log`、`.build/packet-udp-full-kvm.log` | Running 与 Standby 各在 STOP_COMMIT 前后并发发起多个授权访问和包，核对前期 abort、后期唯一恢复、hold 绑新 epoch 及无业务数据重放。 |
| A32 | 冻结测量、开发实机，部分 | 冻结 Worker 将过期 v1 保持 suspect，v2 标 expired，未知 activity 仍阻止睡眠。开发公共续租与 stale revision 冲突通过，受保护父子保有独立 leases。 `managed/lifecycle.log`、`.build/wp10/debian-lease-and-protected-fork-kvm-v2.log` | 子 VM 重放父 lease token 和旧 epoch token 的公网请求，及 UI 对 suspect、expired、unknown 的实机解释。 |
| A33 | 开发实机，部分 | 两路 Serve 压力共享预算，均受保护时拒绝六路并发 launch。实际优先级 Hibernate 记录了 cgroup 采样和回收量。另验证仍有活 child 时 broker 可保留并重接 sealed RAM。 `.build/worker-pressure-kvm-v5.log`、`.build/wp23/tracked-provider-kvm.log` | 联合 restore 风暴、balloon/有界 cache reclaim、共享 backing 末引用，核对 durable 根切换后的真实 memfd/内存回收与全部保护时告警。 |
| A34 | 开发实机，部分 | platformd SIGKILL 后新实例只释放旧连接持有的 hold，独立租约存活并拒绝晚到的 admission。broker 重启恢复真实 VMM 和 sealed descriptor。 `.build/wp23/platform-connection-recovery-kvm-v2.log`、`.build/wp23/tracked-provider-kvm.log` | Worker/controller 重启同时覆盖丢 ACK、旧 collector seq、活动证据变未知和 pending wake，在恢复后核对自动操作只生效一次。 |
| A35 | 开发实机，部分 | 受保护的 System 源在 `auto_hibernate=false`、`keep_running=true`、有效 lease 和活动任务等待时主动 Fork；两子原任务保留 PID 与 nonce，继承策略，独立 lease 与回复。 `.build/wp10/debian-lease-and-protected-fork-kvm-v2.log` | 保持真实模型 SSE 活跃时同一源主动 Fork/Batch，并校验子会话的授权及挂起 I/O 边界。 |
| A36 | 开发实机，部分 | 禁止隐式 API wake 后 Running 仍可接受已授权 lease，而 Hibernate 保持休眠；开启时 lease 从原保存点唯一恢复。私网 TCP 在允许条件下唤醒，策略关闭时保持停驻。 `.build/wp10/debian-lease-and-protected-fork-kvm-v2.log`、`.build/packet-udp-full-kvm.log` | 真实 Stopped/Hibernated 分支、显式手动 Wake、恢复失败后的冷启动禁止，以及旧 wake demand 与手动 Stop 的竞态。 |

当前正式发行门采用 [验收定义](ACCEPTANCE_AND_OPERATIONS.md#10-发行完成清单) 的能力声明规则。上表的待验项仍需要对应的实验或明确的发布范围决定。
