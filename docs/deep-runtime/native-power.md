# 平台原生 Standby 与 Wake 实测

2026-11-25 用 `platformd`、schema 9 Worker 和隔离的 Linux x86_64 KVM Guest 运行平台 API 验收。测试从已在私有 Serve 创建、启动且带平台绑定标签的 OCI 实例开始。平台从实际 Worker inventory 发现并保存执行对象、incarnation 和 epoch。该实例的 inventory 报告已测量的 Guest helper 与可停驻的设备拓扑。Guest 管理状态仍为 `unknown` 时，平台正确显示 VM `running`。

1. 通过平台 `POST /v1/machines/{id}/standby` 提交请求。Worker 的同 ID receipt 到达 `COMPLETED` 和 `STANDBY`，平台 operation 显示 `applied`，机器显示 `standby`。
2. 停止并重新启动 `platformd`。使用原始修订、epoch 和幂等键重放请求，返回同一已完成 operation，机器仍为 `standby`。
3. 强制停止 Worker。通过平台提交 Wake。Worker 不可用时，同一个 Wake intent 保留在预检阶段，effect 为 `none`。
4. 从原 catalog 重新启动 Worker，读取真实 Health 后观察同一 Wake operation 完成。平台和 Worker 均报告 `running`。VMM PID、incarnation 和 execution epoch 保持一致。

隔离运行脚本和证据位于工作区的 `.build/enrolled-platform-power.py` 与 `.build/network-gate-libs/platform-native-power-combined.log`。集成使用 `combined-serve`、`combined-worker`、`live-topology.so.2` 和测量 helper，OCI 根目录基于已验收的冻结 Bundle。测试的原生操作走组合构建，冻结 `dbe526e` Bundle 的验收记录单独保存。

Go 测试同时验证了未验证能力的入队拒绝、Worker 预检断开时原操作等待、提交已生效但回复丢失后重启平台读取同一 receipt，以及拒绝其他 incarnation 的观察。平台当前通过该接口暴露受管 Standby 和同一执行内的 Wake。其他完整状态操作按各自的运行时证据启用。
