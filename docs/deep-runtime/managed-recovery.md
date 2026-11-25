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
