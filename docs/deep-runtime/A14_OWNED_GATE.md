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

## 1eb5104 安装结果

正式安装 Bundle `sha256:b00c0f05bc54d69bbef854ea16c3cac42d36ddc9f8413a1cedd5e5e290434f97`，平台 `c095c4f625c85000bee6de656c582a63ff0e8db7`，Debian System 盘 `sha256:f7b0611ebb21af630addeddf95717aae284f7aa44b4abe2ee7c3933919570b02`。以下用例均从安装目录只读挂载该 Bundle。

| 切点 | 观察 |
| --- | --- |
| `authority_prepared` | PID 退出时 revision 仍为 1，父机继续，自有接收器从失败目标收到 0 条。源 OCI 容器保持 PID 1 和同一 PID namespace，正常业务计数由 2 增至 15。另一批成功子机的继承计数在现场推进到 20，首条容器外发为 21，带新 IP、hostname 和 machine ID。独立 slice 峰值为 379871232 字节。`installed-a14-authority-prepared-oci-loop.log`，SHA-256 `556ee0361ce9b4bb80870638574bcc6f1166e22ece924a147764ff33754de8bc`。 |
| `offered` | PID 退出时 revision 仍为 2，原进程和租约回收。失败实例未发布，父机和另一批成功子机完成回环和私网业务。独立 slice 峰值为 370728960 字节。`installed-a14-offered.log`，SHA-256 `125f9e64626c367e1eef5917ca79ba3ab81e2d84a93b7d93e8c3fb14b9ee6afb`。 |
| `applying` | 退出前后均为 revision 3，公开 operation 等待 120 s 超时。`installed-a14-applying.log`，SHA-256 `046a9c2905434f0dea83888a3b81e2ae21dca93bd38bcef7614b0241bf23b538`。 |
| `identity_ready` | 退出前后均为 revision 4，公开 operation 等待 120 s 超时。`installed-a14-identity_ready.log`，SHA-256 `fe3e2810150373132093d3b7012a9e54ee6da9dedbd027aeed3af64ddeb25ef3`。 |
| `committing` | 退出前后均为 revision 5，公开 operation 等待 120 s 超时。另一次只读现场观察证明已退出的目标仍为 Worker 登记的 `NEW/registered`，其遗留控制路径仍是 Unix socket。`installed-a14-committing.log`，SHA-256 `77fa45ad81da8fbda77ccf9d88b2f59bec3ac557b2c88c002cdb1ac27861e95c`。只读现场记录 `installed-a14-committing-inflight-proven.log`，SHA-256 `53007be8c4b327f0f2eb6ae67be9ce17f2183c9038cdf9374bf960f9f2be9995`。 |
| `policy_armed` | 退出前后均为 revision 6，公开 operation 等待 120 s 超时。`installed-a14-policy_armed.log`，SHA-256 `6f767f0fc1be074b46d653bf1a8b118a3ad2c74c3aa2d1c73fb7c6ba22071eed`。 |

原始记录位于 `.build/release-1eb5104/`。首次容器试验在成功子机的容器 FIFO 请求等待超时，保留 `installed-a14-authority-prepared-oci.log`。改用正常的本机 UDP 请求并核对容器实时计数后，从同一个安装重试通过。一次 `policy_armed` 运行连续观察到前五个阶段，读取下界依次为 36、183、156、190 和 21 ms。目标在第一次看到 `policy_armed` 时已经被选中退出，只有一次该阶段读数。全程使用正常的 Fork 进程，时序由只读目录采样。

## 目标退出的开发回归

Serve 修复 `695dd571d45b0f3bb403775da8ac04c8298e6475` 在控制路径存在但无法重新绑定时，请 Worker 核验原执行的退出。开发 Bundle `sha256:e76b07d2291545e29ec0ee50ad095d5c3c2f1fd795cc4ae424043edb264d7e48` 使用已安装组合的 Worker、libkrun 和 Guest 工具。`applying` 命中后成功收敛，源和另一批目标均完成业务。原始记录 `dev-owned-target-exit-applying.log`，SHA-256 `eea20496f0a05f6af6c2e170b0ac4c5ce9b0fd9d4bb554ee5bff9ce77af05b7f`。`identity_ready` 失败目标已收敛，其后另一批成功目标的一笔 UDP 正常收据未在 8 s 内观察到。该次记录为 `dev-owned-target-exit-identity_ready.log`。

在同一开发包中，`committing` 和 `policy_armed` 均再次命中。Worker 已将原执行写为 `exited`，回收 TAP 和 writer，身份记录保留 `failed_held/commit`。终态检查仍拒绝失败收敛，分别见 `dev-owned-target-exit-committing.log` 和 `dev-owned-target-exit-policy_armed.log`。Worker 修复 `010c38ef91f835c1649606c5e4ac5382cc2cea89` 在同一代际的退出与资源围栏已成立、没有已记录的 TAP 发布时允许该 operation 写为 `ABORTED`。已经记录的激活 commit 或完成的发布仍保留原记录。

使用上述两个修复的开发 Bundle 为 `sha256:b76ce971ad42ca2865e32978162ce657f762283280c0f2eeb7adf054552992de`。Serve SHA-256 为 `7daff9c26298f9409bd5cdf5c1d444130fbc5773dca1c051d0b102a3d8e9d2cf`，Worker 为 `c6dcccb773c2bac1a82d847a42d383f058944fa103dd298e26badbbb94055034`，与已装包字节一致的 libkrun 为 `1e8015e433a7baeb70dd8823d53b9c0a73529ecf08d450a92ae82c79eec8abdc`。平台和 Debian 盘沿用安装试验的输入，夹具为 `e6440f811efc87713a98a54351962ef8df23e625`。带 `v2` 的记录在握手即退出，其 Serve 构建缺少 `deep-runtime`。以下为使用已验证 Serve 的 `v3` 重试。

| 切点 | 开发组合观察 | 原始日志 SHA-256 |
| --- | --- | --- |
| `authority_prepared` | revision 1。目标 `ABORTED/failed_held`，父机普通进程和容器持续运行。容器业务计数由 2 增至 13。另一批成功子机容器仍在原 PID namespace，判定计数实时推进到 18，首条容器外发的计数为 19，携带新 IP、hostname 和 machine ID。 | `dev-owned-target-exit-v3-authority_prepared.log` `ddccc0377a0cf7f7b9f2f223e66129a71248f7ba4088c7e96a278ab03c229311` |
| `offered` | revision 2。目标 `ABORTED/failed_held`，父机继续。另一批成功子机首条新身份私网数据的计数为 16。随后本机回环业务与接收端均记录同一请求 ID。 | `dev-owned-target-exit-v3-offered.log` `8a142925cc4c429436b636a984c044bb80588dcea4847b8542224cccf518e472` |
| `applying` | revision 3。目标 `ABORTED/failed_held`，父机继续。另一批成功子机首条新身份私网数据的计数为 17。随后本机回环业务与接收端均记录同一请求 ID。 | `dev-owned-target-exit-v3-applying.log` `97a7374b2820056e90095118b1f9b36c3d26e85b14a0c15b5f05096a21ceaa89` |
| `identity_ready` | revision 4。目标 `ABORTED/failed_held`，父机继续。另一批成功子机首条新身份私网数据的计数为 19。随后本机回环业务与接收端均记录同一请求 ID。 | `dev-owned-target-exit-v3-identity_ready.log` `a4f62ffd03f297b1b4fe4e4f9a23618776e217b150b5a0143230a45b483a995f` |
| `committing` | revision 5。目标 `ABORTED/failed_held`，保留 `commit`，writer 已释放、TAP 已退休且无已记录发布。父机继续。另一批成功子机的私网接收端有新身份心跳 18–21，本机回环得到计数 22。该笔 UDP 对应的外部收据等待 8 s 超时。 | `dev-owned-target-exit-v3-committing.log` `5676c6b5d00dc6e588771c47d24e7962bb7af4ca214e71db5576c583d0b3f3da` |
| `policy_armed` | revision 6。目标 `ABORTED/failed_held`，保留 `commit`，writer 已释放、TAP 已退休且无已记录发布。父机继续。另一批成功子机的私网接收端有新身份心跳 18–21，本机回环得到计数 23。该笔 UDP 对应的外部收据等待 8 s 超时。 | `dev-owned-target-exit-v3-policy_armed.log` `b2271e32f15c55105a02f8cd911550acd6d17f7a2e837f2d470ab4b516303ecd` |

接收端在 `committing` 和 `policy_armed` 退出观察期间都未收到失败目标地址的数据。`commit` 已持久化的目标可能在退出前收到放行，因此这些接收端观测仅证明本次业务收件结果。Worker 后续提交 `b75b1b743e9fcd6c06cc241a9211cf0f44a97ca2` 将当次执行已记录的 `activation_decision` 带入 Fork 回执，使平台可以独立判断业务结果的不确定性。Worker 的 7 项 Catalog 单测通过。

## 公开操作的放行结果

平台 `b66f5d9` 与带新 Worker 回执的开发 Bundle `sha256:0868d46265940ba7d082cdb2dd1d61ce62d95844aef00ae173171bf4f4b7f3d1` 在同一 Debian 盘上再次运行。两次目标进程经 pidfd 确认退出，writer 释放、TAP 退休，父机继续。退出前、退出后以及写入终态时的三份决议均与同一子尝试匹配。原始记录位于 `.build/a14-public-effect/`。

| 命中切点 | 持久决议与公开结果 | 随后正常子机的独立业务观测 | 日志和 SHA-256 |
| --- | --- | --- | --- |
| `authority_prepared`，revision 1 | 三次均为 `none`，公开终态 `failed`、`effect=none` | 接收器记录心跳 83、84。子机回环确认业务 100，本机持续发送至 120，外部未收到业务 100。 | `owned-cut-authority_prepared-v1.log` · `cb831c44c6c9f590d5e589d90488670e3e6b3cca76c48e063daaf706509604f9` |
| `policy_armed`，revision 6 | 三次均为 `commit`，公开终态 `failed`、`effect=unknown` | 接收器记录心跳 115、116。子机回环确认业务 132，本机持续发送至 152，外部未收到业务 132。 | `owned-cut-policy_armed-v2.log` · `f241ca60a6e04c823402d4da9a4ed8747f6ad0c1a9690f2af3bfdad071470f78` |

`policy_armed` 首轮在 45 秒内没有命中，日志保存在 `owned-cut-policy_armed-v1.log`，表中一轮的阶段观测窗口为 90 秒。两次完整用例均在正常子机的外部业务回执处超时。后续验收另要求正常子机跨至少 12 秒完成三次不同的业务请求，并逐个同接收器的记录匹配。
