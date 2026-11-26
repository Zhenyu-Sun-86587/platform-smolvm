# A14 自有 Guest 的身份门切点

切点夹具通过公开 Fork 创建自有目标。它只读取对应批次的 Worker 目录，用当前 boot、执行代际和 PID 起始时间确认目标。目标退出后，夹具再次读取激活记录。只有退出前后的阶段和 revision 一致，或者尾状态直接承接同一 revision，才记录为命中。未命中时退出码为 2。

源 System VM 和独立接收 VM 始终保留。故障批次只启动一个目标，回收结束后在同一源上提交另一批成功 Fork。每次最多同时运行三台 512 MiB Guest。源和成功目标的同一业务进程通过本机回环响应，并经私网向自有接收器发送普通 UDP 心跳。接收器记录实际来源地址、进程计数、hostname 和 machine ID。

## 2026-11-26 实机校准

使用 f2df146 的授权测量 Bundle `sha256:1d79bf8139035f924f8132fe58837686d6da9a0c72bc2c3ee2f32829882bedff`、平台 `83b3d297e785a7c67c68c94f3f51d475481c4cb8`、Debian System 盘 `sha256:4b5ba5301eac04a398f50fb5be2f969c6e793845d249303783dc2f69a3a3c23f`。

| 阶段 | 证据 |
| --- | --- |
| `authority_prepared` | pidfd 确认退出时仍为 revision 1，决议为 `none`。终态 `failed_held`，TAP 租约已退休，失败实例没有出现在公开机器列表。自有接收器从失败地址收到 0 条业务，父机使用原代际和原身份继续响应。另一批成功子机的第一条外部心跳已携带新来源地址、新 hostname 和新 machine ID，继承计数为 14。 |
| `offered`、`applying`、`identity_ready`、`committing`、`policy_armed` | 待实机。 |

原始记录 `.build/release-f2df146/owned-gate-authority_prepared-calibration.log`，SHA-256 为 `e911a5692ad1569df6efc860038ed887d0ea353ec966ef584e54e68eb7c2e0b2`。故障和成功的公开 Fork operation 分别记录，既有同批成功兄弟证据见总台账。范围只覆盖自有设备正常行为及挂起目标的计划退出。旧 ACK 重放、伪造身份和对关闭身份门的探测均被排除。

```sh
sudo python3 -B tests/integration/owned_gate_cut_kvm.py \
  --bundle "$bundle" --platformd "$platformd" --system-image "$system_disk" \
  --phase authority_prepared
```

传入已安装 Bundle 时使用 `--use-installation`，夹具通过只读 bind 挂载原件。

在代表切点传入 `--container` 可同时检查 OCI 容器。夹具取同一 Bundle 的 OCI 模板内 crun、musl 和依赖，为 System 源启动一个新的 PID 和 mount namespace。容器进程持续写本地计数，并在自有工作负载提交正常请求后向同一个接收器发送数据。夹具核对源和成功子机的容器 PID 1、继承的 namespace 与计数，以及成功子机第一次业务发送的真实来源地址、hostname 和 machine ID。
