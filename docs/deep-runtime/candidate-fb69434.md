# 冻结候选 fb69434 的安装与 KVM 验收

2026-11-26 在隔离的 Linux x86_64 Worker 上构建、打包并安装 Bundle `sha256:a9b41917173a0eea2306aa41276b59fa314e1d54f9d52f45766bcf2ce7698c35`。安装目录为 `/var/tmp/deepfork-smolvm-build-cache/release-fb69434/installed`，工作区证据在 `.build/release-fb69434/`。

## 冻结输入

| 输入 | 固定值 |
| --- | --- |
| smolvm | `fb6943480163b9e7fb8cd6626f92a44cf0dc2326` |
| libkrun | `9246cbb05074df7aebea1b9b1d1ff1a5c067d4c6` |
| libkrunfw | `4d5852a59f9c8bc75780544d0fe93b5015343a32` |
| 平台及验收夹具 | `c011476d0f533d606dd3c6cd09ad2f0c532bdfa9` |
| 平台二进制 | SHA-256 `0b9b27678863d8bf6ecfb7df6409b2765112dc92dc88a3a7da6db853e01948c6` |
| 内核源码归档 | Linux 6.18.60，SHA-256 `3c2f44ef6acc021a7ad444d9a2e6ec1ef488ff09368793d0004a6080f69d621e` |
| 构建锁 | SHA-256 `95a4049d38c419de22779b454f4de291ef10c1455abdb176f15e621e356e7c5a` |
| 工具链锁 | SHA-256 `32943b623419022f5fba0908602962791d5aa5a214a67503eec9c8f85992433f` |
| 运行时构建记录 | SHA-256 `ae028877105a072c7dad6e475b3d69b16cea5aa48ae397d7714c6d1961ccf5b9` |
| Debian System 盘 | 805306368 字节，SHA-256 `065e82d9053c8a4a1ed6ba9030ccd816b3791f498576648a9a11010ee33b281d` |
| System Agent | SHA-256 `9da603e740a326ab2c13244703f7ce85be9f77598962255ef01ab7b3dabb565f` |
| System identity-helper | SHA-256 `424f7836ee4648a0d00399a5103daea3ff8bdf45d2508e8717d638bfcfbbc8ee` |

新内核用 `deep-x86_64.config` 和 `managed-identity-x86_64.config` 两片配置在独立暂存中编译。Serve、Worker 和 libkrun 的最高 glibc 符号版本均为 2.30。System 盘使用已校验 ext4 副本，其中两个 Guest 工具与新构建结果逐字节相同，文件系统检查通过。公开验收的暂存文件在私有 mount namespace 的 tmpfs 内，各组创建独立 Worker 状态目录，VMM 归入试验专属的 cgroup。打包阶段也使用私有暂存，安装目录由锁定安装器在持久文件系统上原子发布。

## 九项包内测量

用冻结平台的 `measure-managed.py --private-network --live-resources` 运行 KVM。基础组、网络组、资源组从同一测量 Bundle `sha256:7681ac8c72c819d931c05d721e490ede26db61131bd698c8940a6ecb7fc83884` 启动。包内 `metadata/managed-acceptance.json` 使用 `RuntimeManagedAcceptance/3`，SHA-256 为 `db8e7b643426339fe8aa01e5970d95a0510e7323cbba34ff3ffad71a5934ef38`。它绑定同一组运行时工件和 Debian System 盘，并记录每份日志及夹具的摘要。

| 包内日志 | 实测范围 |
| --- | --- |
| `oci-capture.log` | OCI 双子机的两盘和 RAM 切点、父退出后递归 Fork、激活前失败子独立重试、Checkpoint、Hibernate、exact Restore 与 Rollback。 |
| `lifecycle.log` | v1/v2 租约、出站活动保护、Standby 至 Hibernate、两个离线需求共用一次精确恢复并重新授权。 |
| `system-fork.log` | 真实 System Fork 关闭原流，两独立 Agent socket 和 CID 确认。 |
| `system-captures.log` | System 持久 StatePoint 恢复和 Hibernate 恢复，验证新 CID。 |
| `system-relay.log` | 真实 Guest relay 执行身份、单次 Exec 和撤销。 |
| `packet-native.log` | FixedInternal → Native，验证私网 TCP 唤醒、持久点精确恢复、有界 UDP 首包交付和超界数据包丢弃。 |
| `packet-fixed.log` | Native → FixedInternal，重复 TCP、持久恢复和有界 UDP 验证。 |
| `live-resources.log` | parked-capture 的公开 CPU 和 balloon。platformd 等待 balloon 时退出，重启后原操作分别报告 CPU 已应用和 balloon 被拒。 |
| `live-resources-hot.log` | live-compute 公开热增至 2 vCPU、640 MiB，并核对 16 MiB balloon 与 1500 毫核配额。清除配额后恢复两个 vCPU 槽的内核额度。 |

首次分配的 5 GiB 暂存在 Native 方向的重复 Hibernate 中触发 Worker 的存储准入。Worker 请求 608,174,080 字节，当时可预留 16,384 字节。随后用 8 GiB 私有暂存从头运行九组。原始拒绝日志保存在 `out/managed-measurement.initial-5g/metadata/managed/packet-native.log`，SHA-256 为 `25ce4587ba574c1a8fda635ed2b00cb6bd6cf8897a927c5967d0fe05323184e4`。

## 安装与能力握手

| 验收 | 实测结果 |
| --- | --- |
| 原子安装 | `pack-candidate.py` 生成候选，随后 `bundle.py install` 原子发布。以期待的 Bundle 摘要独立运行 `bundle.py verify` 再次通过。 |
| OCI 启动 | 打包前和安装后各启动两次真实 OCI Guest。两份 `RuntimeOciBootEvidence/1` 逐字节相同，SHA-256 为 `23541144c34d9f63c5498a972980f7df2f43c3ab940a082c3beba6d15ca412f2`。 |
| 运行进程身份 | Serve 和 root Worker 均从同一安装目录启动。核对 `/proc/<pid>/exe`、Worker socket 的 `SO_PEERCRED`、Worker Health 和 Bundle。Serve SHA-256 为 `1f0077cac48fd335425a559d690b36278375984ecabe8b14d6c1cef23923ad82`，Worker 为 `0c6454b969bd304fb98668ff963b9dc4a2b3703acd4228ea9762b8cd326f7d9c`。 |
| 发布能力 | Native、FixedInternal、TAP 和 TAP restore 均为 `managed-private-system-v1`。Standby 与 Wake 为 `managed-parked-v1`，System restore 为 `system-debian-v1`，live-compute 为 `managed-live-compute-v1`。`network.native_reidentity` 状态为 `unknown`。 |

打包、安装和原目录握手记录为 `pack.log`、`install.log` 和 `out/installed-handshake.json`。握手 SHA-256 为 `7818e537d30a5cf5227e149d7812b759679d2dcb8c54c0ac2a49cbf5acbe77f2`。

安装归档解压后的 ext4 与测量原盘字节相同。按已验证的运行时工件逐项核对库阶段后，库和 System 原盘工作副本移至专属 `/dev/shm`，继续从原路径访问。安装件和构建记录持久保留，核对见 `relocated-work-copies.log`。

## 安装后公开操作

以下用例使用同一安装 Bundle 和表中固定的 `platformd`。任务、保存唤醒和卷生命周期使用包内归档对应的 Debian System 盘。首桌面和完整 Stop 使用带 Xvfb、xterm、x11vnc 的独立 Debian 盘副本。该副本 SHA-256 为 `f98f4c635fc7de1a3ee8bba28ab4185b3fdf5734004cf78c14e868848e053cb9`，已写入并复核同两件冻结 Guest 工具。

| 用例 | 结果 | 原始日志和 SHA-256 |
| --- | --- | --- |
| `task-fork-cancel-retry` | 通过。公开取消保留已提交 child 和原 capture。父机 Stop、Delete 并重启平台后，只重试原失败的 child，保持其 ID 和原 capture，已取消的 child 继续围栏。Task 认领返回同一 run、Guest PID 629 和原随机标记。 | `installed-task-fork-cancel-retry.log`，`2e56ebf3586eb8462f14497a16a6447976c9f2119b8454ba21a16ccf80e454d5` |
| 卷生命周期 | 通过。在线修改挂接和增长受围栏。公开 Stop 后，16 MiB 文件系统在 32 MiB 块设备上读取原文件，再由 Guest 扩至 32 MiB。原持久卷转挂到另一真实 KVM，并读回原字节。 | `installed-stop-volume-kvm.log`，`17e14fcf84550fc0b0cac6dddd7c207a0c592d1be13c84f2252bdf8f20d8ccbf` |
| 手动 Stop | 通过。普通 Agent 停用时完成 Stop，确认旧执行、writer 和待处理 wake demand 受到围栏。重新冷启核对根盘、挂载卷、裸盘写入及旧桌面围栏。原操作同键重放稳定，旧执行上的新 Stop 被拒。Hibernated 上的 Stop 返回 `STATE_CONFLICT`，显式唤醒恢复原 RAM 标记。 | `installed-system-stop-kvm.log`，`0da93fa0631c776ea27cb3ac533d3f85d625a0842299ae51f9274bb69ad000e1` |
| 首桌面 | 通过。关闭触发器或撤销凭据时保持 epoch 0 空槽。两个并发会话只启动一个 epoch 1 执行，同键复用 v2 租约，确认中撤权回收租约。真实 Debian 经单次 Gateway 票传出 800×480 画面。 | `installed-desktop-first-cold-kvm.log`，`b5d4c4f4bdd446e0245b3b1bd242afbb224efacb2809632302bd4227d5a743c8` |
| `saved-wake-failure` | 部分通过。保存清单不可读和未发布恢复 VMM 退出都返回 `STATEPOINT_RESTORE_FAILED`，保留同一 Hibernated 保存点，后者消耗一次 epoch。Worker 拒绝冷启。随后显式 exact 重试返回 `VOLUME_GENERATION_MISMATCH`。 | `installed-saved-wake-failure.log`，`cfa6782db3e817ff55b1455c7fe0340248bbe8e9274f918de08598c060273256` |

在同一安装件用只读 SQL 探针定向复跑后，确认目标在获得 writer grant 且身份仍为 `authority_prepared/none` 时退出。原盘仍指向同一个 next head，失败 operation 为 `ABORTED`，writer grant 已 `released`，执行为 `exited`。此版仍以这条 grant 历史触发代际冲突。原查询、回执与失败边界见 `installed-saved-wake-writer-observed.log`，SHA-256 为 `d411181c1bfa3129cdedfa480dd12ddbb5911949bb5efea76d4d1ae6307da6a7`。

### 保存唤醒修复的开发复测

Worker 修复 `3e517f85fffe7af52c13fa4deb3371de3e0a5a60` 允许在原保存点上重试已经确认退出、释放 writer 且尚未开始 Guest 身份应用的 exact 尝试。身份 BEGIN 在 helper 获准写盘前持久记录，BEGIN 之后、授权未知或仍有 writer 的尝试继续触发冲突。Rust 定向回归验证这些边界，库测试记录为 12 通过、3 个网络特权用例按配置忽略。日志为 `restore-retry-worker-tests.log`，SHA-256 为 `a0b8a0945477cfd1224a12ab5effdba30417eb466e4b84ef48485c357471fb37`。

平台夹具 `1b44feca90c056245bc5297053792a5ce64eb9a0` 等待真实 writer grant 发放，在 Guest 身份 BEGIN 前终止目标，核对 `released/exited` 后再显式重试。同一夹具在 fb69434 安装件仍复现原错误，见 `installed-saved-wake-tracked-regression.log`，SHA-256 为 `322b44ad9b3e1f3b0480cca459f73a161a9fc4896434808ef669216267813666`。修复后的开发组合 `sha256:b7127898b736a835f7fe513fff14c0e65bc88da7b759d63ff56837cd50af5abd` 通过全部步骤，并在下一 epoch 恢复原 RAM 标记。开发日志为 `development-saved-wake-writer-fixed.log`，SHA-256 为 `312707c82e11583810062648001d3c58f2e925eafb1b2c07f5c13f2563aefd20`。

## 历史版本复核

同一验证器确认 `91e2c72` v1、`d8cc5d5` v2 和 `86dae32` v3 原安装树的摘要。首次复查 `d8cc5d5` 时，旧开发夹具对该安装件创建的硬链接触发了单链接校验，原打包树仍验证通过。开发夹具改用独立缓存后，原安装 inode 和字节保留，安装树再次通过，摘要为 `sha256:c5070578cdf0d4fd01cc120721515c169e9297a3f624803def398996c0e249b5`。工作区日志为 `.build/release-fb69434/historical-reverification.log` 和 `historical-d8-installed-reverified.log`。

[86dae32 安装记录](candidate-86dae32.md) · [d8cc5d5 安装记录](candidate-d8cc5d5.md) · [91e2c72 安装记录](candidate-91e2c72.md) · [逐项验收台账](acceptance-ledger.md)

### 首次 Fork 回执丢失

在原安装和配套 platformd 上，从 Debian 源发起公开 Fork。客户端观察到 TCP 接收缓冲已有 HTTP 回复即关闭连接，未读取操作或子机 ID。重启 platformd 后，按原控制代际、原修订和原幂等键重试，得到唯一操作 `op_311a5dfe6e2e68fd077b96fb46ee74ac`。Worker 目录只有一个批次、一个 capture `ram-9417e942c0ab19272bd3d5831b3dd6ea` 和一次目标执行。子机从原 RAM 读回标记 `bccba8ec0d8b44a688030e5dddd48107d34eb915`。完成后重放仍返回同一操作，修改子机名后同键请求返回 409，原请求摘要不变。

使用 `managed_system_vsock.py --case fork-lost-reply` 并传入同批安装、平台与 System 根盘。日志 `.build/wp23/a18-lost-public-reply-fb69434-v2.log` 的 SHA-256 为 `2dfc5e4022e26997cda965034c46ac2e964146fea41ce1f75810c2c88218c8ef`。[复现夹具](../../tests/integration/managed_system_vsock.py)
