# 托管分叉和持久恢复实测

2026-11-25 在 x86_64 KVM Worker 上使用开发 Bundle `sha256:3b1be2e1518a8a2027662e2fe79cd6a318e1d2b483cc5ef42e8f0b1d5594c22c` 验证。Guest 有三个块设备，一个持续运行的进程每次写入两块可写盘并分别 fsync，输出 PID、计数、主机名、machine-id 和两盘前一次写入值。

| 场景 | 实际观察 |
| --- | --- |
| 一次分叉、两个目标 | 子机恢复同一计数边界，在独立可写 head 上继续。终止父机后，处于 Standby 的子机再次分叉，源仍保持 Standby。终止两台同批子机后，孙机继续原进程并通过两盘数据校验。 |
| 同批部分失败 | 在目标的激活提交前，用 pidfd 终止一台目标。显式重试复用原捕获和逻辑子机 ID，只推进失败目标的 epoch，成功目标的收据保持一致。 |
| Checkpoint、Hibernate、恢复 | Checkpoint 发布持久 manifest 后源继续运行。Standby 源休眠时计数保持不动。exact 恢复后同 PID 从下一次计数继续，两块可写卷保留原 ID。 |
| Rollback、独立恢复 | 回滚先保存救援点并确认旧执行退出，再以新 epoch 恢复早先的计数和磁盘。对同一 checkpoint 执行独立恢复，两个目标有各自的写入层。 |
| 平台公开 API | 把真实三盘 OCI 的执行登记为平台绑定后，经公开入口验证 Fork Batch、Checkpoint、独立恢复、Hibernate 和 Rollback。重启 platformd 并重放请求仍返回原批次和两个原子机 ID。休眠后 `start` 精确恢复到 epoch 2，再回滚到 epoch 3。 |

运行时复现入口为 smolvm 仓库中的 `tests/test_deep_managed_launch.py --fork-faults --statepoint --bundle <目录> --guest <计数器>`。本地原始记录位于工作区下的 `.build/network-gate-libs/fork-point-integrated-v3.log`、`.build/platform-native-oci-fork.log` 和 `.build/wp10/statepoint-platform-oci-v2.log`。

System 另行验证。带 Agent 的 System 镜像使用 vsock，开发 Bundle 缺少该设备的 parked rebind 合同时会拒绝捕获。支持范围以每台源机的实测设备清单为准。

## 运行中的 StatePoint reader 跨 Worker 重启

2026-11-26 从冻结安装 `d8cc5d5` 的持久点恢复 VMM，再用 `SIGKILL` 终止 Worker。替换 Worker 核对仍存活的原 PID、只读 RAM 的原始 inode 及其 1376124928 字节内容，重新取得原 reader 与同一租约。租约修订从 2 变为 4，Worker 回到 `ready`。用旧安装 `91e2c72` 重跑，VMM 缺少重新导出 FD 的能力，租约保持 `unknown`，Worker 保持 `recovering`。两次均使用各自安装的 VMM 和 reader。

复现入口为 `tests/integration/worker_restart_kvm.py` 的 `durable-reader` 和 `legacy-reader-no-reexport`。实机原始记录分别位于 `.build/wp13-tracked-durable-reader-kvm.log` 和 `.build/wp13-tracked-old-reader-negative-kvm.log`。

## 完整分叉与独立恢复后的任务索引

2026-11-26 使用已安装的 `d8cc5d5` 运行时和开发平台，在 Debian Guest 上从公开 API 启动持久任务。任务生成随机标记并等待输入。两组实测均在确认原 VMM 的 pidfd 已退出并重启 platformd 后，从目标机器自己的任务 API 读回输出。

| 操作 | 结果 |
| --- | --- |
| 原生 Fork Batch | 暂停普通 Agent 后，两个请求子机中有一个成功，另一个超出预算。重复提交返回同一批次。完成后取消返回 409，capture 与子机记录一致。子机最初报告任务索引不可用。Agent 恢复后，目标任务拥有自己的 ID 和命名空间，保留原 run ID、源任务 ID 和源机器 ID。原 PID 661 打印了原随机标记与子机收到的回复。 |
| 独立 VM checkpoint 恢复 | 源任务等待时捕获，使用 `clone_conflicts` 恢复为新机器。独立任务索引与公开输出保留原 run ID、PID 656 和随机标记。 |
| Task 唤醒 | 公开任务从 Hibernated 唤醒后在 epoch 2 运行，输出可读，取消获得终态回执。随后从 Standby 提交另一任务，仍在同一执行和 epoch 2 完成。 |

复现入口为 `tests/integration/managed_system_vsock.py` 的 `task-adoption`、`task-restore` 与 `task-wake`。原始记录在 `.build/wp23/tracked-native-task-adoption-kvm.log` 和 `.build/wp23/tracked-public-task-wake-kvm.log`。这些测试验证了源 VMM 退出。公开 Delete 的结果在 A18 单列。
