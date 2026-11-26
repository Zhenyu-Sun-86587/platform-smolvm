# 平台原生 Standby 与 Wake 实测

2026-11-25 用 `platformd`、schema 9 Worker 和隔离的 Linux x86_64 KVM Guest 运行平台 API 验收。测试从已在私有 Serve 创建、启动且带平台绑定标签的 OCI 实例开始。平台从实际 Worker inventory 发现并保存执行对象、incarnation 和 epoch。该实例的 inventory 报告已测量的 Guest helper 与可停驻的设备拓扑。Guest 管理状态仍为 `unknown` 时，平台正确显示 VM `running`。

1. 通过平台 `POST /v1/machines/{id}/standby` 提交请求。Worker 的同 ID receipt 到达 `COMPLETED` 和 `STANDBY`，平台 operation 显示 `applied`，机器显示 `standby`。
2. 停止并重新启动 `platformd`。使用原始修订、epoch 和幂等键重放请求，返回同一已完成 operation，机器仍为 `standby`。
3. 强制停止 Worker。通过平台提交 Wake。Worker 不可用时，同一个 Wake intent 保留在预检阶段，effect 为 `none`。
4. 从原 catalog 重新启动 Worker，读取真实 Health 后观察同一 Wake operation 完成。平台和 Worker 均报告 `running`。VMM PID、incarnation 和 execution epoch 保持一致。

隔离运行脚本和证据位于工作区的 `.build/enrolled-platform-power.py` 与 `.build/network-gate-libs/platform-native-power-combined.log`。集成使用 `combined-serve`、`combined-worker`、`live-topology.so.2` 和测量 helper，OCI 根目录基于已验收的冻结 Bundle。测试的原生操作走组合构建，冻结 `dbe526e` Bundle 的验收记录单独保存。

Go 测试同时验证了未验证能力的入队拒绝、Worker 预检断开时原操作等待、提交已生效但回复丢失后重启平台读取同一 receipt，以及拒绝其他 incarnation 的观察。平台当前通过该接口暴露受管 Standby 和同一执行内的 Wake。其他完整状态操作按各自的运行时证据启用。

## WP-21 运行中调整 CPU、内存和 balloon

2026-11-26 在真实 KVM 上运行了带 systemd 的 Debian Guest。公开 API 和客户端使用平台 `ab21997`，两种资源配置的公开 API 验收使用开发 Bundle `sha256:b79f24bd67af481dbe80c5ed9d870848b012d523107adb78a309e62043bb99b9`。

| 场景 | 核对结果 |
| --- | --- |
| 可捕获配置 | 客户端将两个已分配的 vCPU 中的一个下线。休眠后精确恢复保留该在线集合，在新执行上再次上线原插槽。32 MiB balloon 同时读到目标值和实际值。 |
| 创建时启用热加配置 | Guest 从 1 vCPU、512 MiB 增长到 2 vCPU、640 MiB，内核的 `MemTotal` 从 525648 增至 656720 KiB。VMM 报告新增 134217728 字节映射且已接入。客户端按能力拒绝此配置的完整捕获。 |
| 进程退出与部分完成 | 在 CPU 已确认、balloon 尚待确认时终止 platformd，重启后原操作记录已生效的 CPU 和未完成的 balloon。另在只收到目标值时重启 Worker，最终恢复旧 balloon 目标并报告失败。 |
| CPU 时间配额 | 公开请求设置 250 millicpus，精确恢复后配额仍生效。清除后恢复两个分配插槽的配额。另一次请求丢失回复后强制重启 Worker，原 PID 的 `cpu.max` 保持 `1002 334000`，对应请求的 3 millicpus，准入仍计两个 vCPU。 |
| 四种客户端 | Go CLI 设置 400 millicpus，Python 清除配额并设置 8 MiB balloon，TypeScript 下线一个 vCPU，Chromium 页面在同一执行上恢复两个在线 vCPU 并把 balloon 调回零。 |

原生行为记录在 `.build/native-resize-integrated-kvm-v2.log`，公开接口和客户端分别记录在 `.build/wp23/public-resize-kvm-v3.log` 和 `.build/wp23/public-resource-clients-kvm.log`。CPU 配额与 Worker 重启记录在 `.build/wp21-tracked-cpu-restart-kvm.log`，旧 Worker 的拒绝与原 cgroup 配额记录在 `.build/wp23/public-resize-historical-partial.log`。自动回收在这次试验中超时，Guest 确认释放量为零。上述开发实机结果单独登记，冻结版本按包内测量记录验收。
