# 多 Worker 控制面与验收记录

多 Worker 已合入平台 `polish/integration@69a42f4`，平台 schema 为 1025。下文 N01–N18 的早期证据来自 `feat/multi-node-v1@1c039e3`，合并后补验了 schema、运行时能力和同机双 Node。测试使用临时 SQLite、真实 mTLS、Unix socket 和隔离夹具。两台独立 x86_64/KVM Worker 的上线验收仍待实机。[合并源码与目录](checkout.md)

## 运行方式

一个 `platformd` 写入中央 SQLite。每台登记的 Worker 运行一个 `sandbox-node`，它验证控制端、保存投递日志、在本机消费 Unix FD，并调用指定的 Serve/resourced。旧安装的对象回填至 `local-worker`，保留主要 ID、Runtime 引用和正常重启前后的 control epoch。进程内 Runtime 继续使用现有本机维护入口。

- **指定或自动创建。** System 模板可以传 `placement: {"mode":"pinned","worker_id":"worker-a"}` 或 `placement: {"mode":"auto"}`。也可用 `sandboxctl machines create` 的 `--worker` 或 `--auto-placement`。控制面在一次写事务中固定 Worker、Binding、Operation 与资源预留。旧 API 指定的进程内 Runtime 仍走本地路径。当前远程创建支持 managed System；远程 managed OCI 创建返回 `NOT_SUPPORTED`。
- **已有对象。** 启停、恢复、Fork 子项及原生 retry 都使用原 Worker。不可变模板须在各 Worker 预先导入，将内容 digest 和 Node 允许的本地 root generation 配对；只有 resourced 证实不可变对象、来源和容量后才会进入候选。独立卷和 StatePoint 保留原 Runtime、Bundle 和 reader。在不同 Worker 上使用相同的 Serve 名、私网名或 Guest IP 不会改变对象的 owner。
- **容量。** 调度使用 `QueryWorkerBudget`、可用内存和 PSI、Runtime capability、私网、已验证的存储和未覆盖的预留。预算查询限时 5 秒，样本有效期 15 秒。可能已发送的动作保留 Unknown 及资源预留，仅向原 Worker 查询同一操作。折叠额外预留需要原生 admission 的证据及同一 host/broker/Node 会话中更晚的预算采样。原生端保留最终准入权。
- **访问。** 管理、服务、桌面和 Guest 请求继续从平台入口进入。Node 端仅接收预定义请求，按 Runtime 与 execution 校验。双向流支持 half-close、背压和撤权。Guest relay 还要求 Worker 验证本地来宾连接，平台复核 grant。服务 HTTP 在请求体发出后断链时返回不确定结果。
- **可观测性。** `GET /v1/workers` 和 `sandboxctl workers get WORKER` 显示 enrollment、fresh budget、期望维护模式、已观察阶段和阻塞项。机器视图分别报告 `worker_id`、`owner_status`、`power_state` 和 `last_confirmed_power_state`。调度拒绝返回逐候选原因；已接受操作可查 `sandboxctl operations placement OPERATION` 或 `GET /v1/operations/{operation}/placement`。

[控制面配置示例](../../packaging/install/config.example.yaml) · [Node 配置示例](../../packaging/systemd/sandbox-node.example.yaml) · [管理 API](../../api/openapi.yaml)

## 登记、下线与恢复

1. 为每台 Worker 分配不同的 `worker_id`、Node 密钥和全局 `runtime_instance_id`。Node 的 `instance_id` 写本地 Serve 名。平台配置的 `local_instance_id` 与它对应。为两端配置同一 Bundle manifest 和 reader。用已认证的 `GET /v1/control` 取得 `installation_id` 和 `control_epoch`。
2. 登记可使用管理员签发的五分钟一次性凭证。Worker 在本地生成密钥和 CSR，经管理 HTTPS 端点领取新的公钥证书。平台只为配置中的 Worker、代际、公钥 pin 与主机名签发，精确重试同一 CSR 可取回原证书。也可使用带外预签发证书。 [登记与换证命令](WORKER-ENROLLMENT.md)

   配置双向 TLS。Node 证书的 SAN 包含端点 DNS/IP 以及 `spiffe://sandbox-platform/node/<worker_id>/<generation>`。控制端证书包含 `spiffe://sandbox-platform/controller/<installation_id>`。两端分别配置对方 CA 和公钥 SPKI 的 SHA-256 pin。Node 的私有状态目录、Serve socket、Worker socket 和可选 Guest relay 目录位于 Worker 上。发布包内附独立 Node 服务单元和样例。在 Worker 上将已校验的 `bin/sandbox-node` 安装到 `/usr/local/libexec/sandbox-node`，将 `systemd/sandbox-node.service` 安装到系统单元目录，然后填写 `/etc/sandbox-node/config.yaml`。该单元只启动 Node。其进程 UID 必须匹配 resourced 的 `admin_uid`，还须具有私有 socket 和 KVM 的访问权限；Worker 客户端会校验 resourced 对端 UID 为 0。
3. 启动配置好的 Node 后，平台校验握手、路由、会话和已知对象，取得新鲜预算才开放调度。首次登记会固定 journal 的持久 ID。已有受管对象或历史预算的 Worker 升级时，先停止原 Node，执行 `sandbox-node -config /etc/sandbox-node/config.yaml -print-journal-id`，将确认值写入平台的 `nodes[].journal_id`。新 Node 进程完成只读对账后才接收新写。相同证书连接到不同 journal 会隔离该 Worker。恢复旧 DB 会更换 control epoch 并进入 maintenance。普通重启保留 epoch。
4. `sandboxctl workers set-mode WORKER cordoned --revision N --reason REASON` 关闭新 Create/Fork。转入 `draining` 后，控制端和 Node 执行入口门禁、有效策略 overlay、逐项停用已登记 packet-wake 规则，并收敛 WakeDemand。collector 保留运行。恢复 `active` 时重新读取当前用户 desired revision。
5. 同一控制端换证时，停 Node，用旧配置和下一代证书执行下面的离线命令。相同安装 ID 和 epoch 的 generation 须恰好加一，保留原 gate。跨控制端或恢复后的 epoch 要求更高 generation。先关闭旧控制端，再离线重绑原 journal；Node 随后进入 Drain，并从本地拒绝旧 authority。随后同步控制面 pin、generation 及 Node 配置，启动 Node，并核对已知对象、恢复阻塞和预算。撤销某一代可使用 `sandboxctl workers revoke-enrollment`。

```sh
/usr/local/libexec/sandbox-node -config /etc/sandbox-node/config.next.yaml \
  -rebind-from /etc/sandbox-node/config.previous.yaml
```

覆盖级别为 `known_only`。现有 Worker 能按已知 ID 查询，并报告聚合预算，无法证明整台 Worker 上不存在外部 CLI 实例或未知唤醒规则。系统在证据不足时保留 `draining_blocked` 及原因，并拒绝宣告 Drained 或 Decommissioned。独立卷、备份、休眠和历史 StatePoint 的 owner/reader 继续保留。手工恢复通过 `sandboxctl admin backup recover-native` 将校验过的闭包写入新目录，`--worker-catalog` 可准备新的独立 catalog。 [原生备份与手工恢复](BACKUPS.md)

## N01–N18 本地证据

下表列出可用 `go test -run` 单独复现的用例。配额、断链、重启与设备回执均有明确测试边界。

| 编号 | 本机覆盖和代表用例 |
| --- | --- |
| N01 | 从 schema 1019 迁移后核对原 ID、外键、epoch 和重复本地 endpoint。不同 Worker 可登记同名 Serve。 `TestWorkerMigrationPreservesPinnedIdentity` |
| N02 | 两个 6 GiB 请求竞争 8 GiB 模拟预算时仅一个获准；第二个合格 Worker 加入后可接新请求。HTTP 指定/自动创建与 Guest grant 创建核对持久 owner、容量和私网。 `TestPlacementSerializesAcceptAndHolds`、`TestHTTPPlacementAndWorkerMaintenance`、`TestPlacementUsesEachRuntimeExactStorageCapacity` |
| N03 | Worker 已受理后丢回包。只查原操作，保留 Unknown 和 hold，另一 Worker 的提交计数为零。 `TestLostResponseQueriesOnlyOriginalWorker`、`TestPlacementUnknownRechecksOriginalAndRequiresCausalBudget` |
| N04 | 重开真实 SQLite Node journal，覆盖发送窗、同 ID 重投、冲突 digest 和原生终态回执。 `TestCrashWindowRebindAndKnownOnly`、`TestJournalLeavesUncertainSendFencedAndTerminalHistoryDoesNotFillReconcile` |
| N05 | WakeDemand ack、真正的 Restore 决策和预算水位分别注入；核对原生 `restore_target`、原 owner 和执行身份。 `TestIngressWakeDemandOnlyFoldsAfterPinnedRestoreAndNewBudget`、`TestRestoreVerifiesNativeTargetReceipt` |
| N06 | 本地/远程 owner fixture 上运行部分成功的批量 Fork、源解绑、取消和原生 retry。核对逻辑 child、capture、attempt 和已成功项，按每个可写设备及有效 child 数计费。 `TestForkBatchAndRetryChargeOriginalWritableCapacity`、`TestNativeForkKeepsPartialChildrenAfterSourceRemoval`、`TestNativeForkCancellationOwnsOnlyUncommittedTargets` |
| N07 | 两路 Node 的相同本地对象名、服务及卷按各自 route 校验；经真实 Unix socket 和 TLS 的 Guest relay 验证 grant、凭据过滤及撤销。 `TestIdentityScopeAndTypedRequests`、`TestVolumeScopeReplayAndAdmittedActions`、`TestRemoteGuestRelayAttestsAndCancelsCredentialProxy` |
| N08 | 回环 TCP 半关闭后仍能收到反向数据；HTTP POST 在上游读完整个请求体后断链，计数仍为一次。 `TestStreamHalfCloseAndControlUnderBackpressure`、`TestRemoteHTTPPostBodyIsForwardedOnceAfterLostResponse` |
| N09 | 活动请求和 running lease 阻止休眠；同一源上手工 Fork 仍经独立准入并到达原 Worker。本例使用等待中的请求和持久租约。 `TestLongRequestProtectsSleepAndAllowsOwnerFork` |
| N10 | 维护期间修改用户策略，解除 overlay 使用新 desired revision。 `TestWorkerPolicyOverlayUsesCurrentDesiredRevisionOnReactivation` |
| N11 | 注入逐规则禁用回执丢失和已派发 wake，验证规则再次观测、collector、恢复顺序与阻塞项。 `TestDrainPacketRuleLostReceiptAndReactivation` |
| N12 | 仅已知对象和聚合证据时保持 blocked，拒绝空节点证明和退役。 `TestKnownOnlyDrainBlocksAndRecoveryRequiresRebind` |
| N13 | 生产注册/轮询路径接两套回环 TLS Node，分别跟踪 host、broker、Node 和 Serve 身份。注入旧会话与重启后的原操作回执，并用同一证书重建空 journal，确认旧身份被隔离。 `TestJournalRestartAndLossFenceWorkerSessions`、`TestControllerEnrollsAndSamplesTwoLoopbackNodes`、`TestFreshRetiredWorkerQuarantinesButOverlappingQueryDoesNot`、`TestNativePowerReconcilesOriginalWorkerOperationAfterRestart` |
| N14 | 模拟重启后的 Hibernate：对账要求原实例、epoch、HIBERNATED 和原 capture 均相符；STOPPED 或错误 capture 保持阻塞。 `TestRebootRequiresOriginalHibernateCapture` |
| N15 | 数据库恢复轮换 epoch。离线换代与恢复重绑校验旧 authority；已建立的长连接也会重查授权。 `TestBackupRestoreChangesEpochWithoutReplayingOldWork`、`TestOfflineEnrollmentRotationKeepsGateAndRecoverRebindDrains`、`TestLongNodeStreamsRenewAuthority` |
| N16 | 卷、StatePoint、备份 capture 校验 owner/Bundle/reader 与原生来源；手工 Restore 依据捕获时的 CPU、内存和可写容量向原 reader 准入。 `TestManualRestoreUsesCapturedResourcesOnOriginalReader`、`TestWorkerPinsRejectCrossNodeResources`、`TestPinnedPlacementCannotOverrideOwnerOrReader`、`TestBackupCapturesRequireOriginalServeAndSourceOperation` |
| N17 | 错证书、重复登记和跨 Serve 的读写被拒绝；一次性凭证支持限定 CSR 换证，路径型备份成员限于已选择的不可变闭包。 `TestWorkerEnrollmentTicketTLSRotationAndRecovery`、`TestWorkerEnrollmentIsUniqueAndRotationIsAtomic`、`TestWorkerRPCRejectsOtherServeOnSameWorkerAndBundle`、`TestRemoteBackupRejectsPathsAndRouteAlias` |
| N18 | 模板校验受单次和总时限约束，轮询游标让后续模板也可取得证明；以阻塞流占满 bulk 配额时控制握手仍可用；接收方独立核对成员长度及 SHA-256，截断和错误摘要均失败。 `TestWarmingRootsLeaveTimeForBudgetAndRotate`、`TestRemoteBackupLimitsLeaveControlAvailable`、`TestRemoteBackupDigestAndIncompleteMembers` |

[NodeGateway 故障用例](../../internal/nodegateway/gateway_test.go) · [调度与维护用例](../../internal/registry/worker_placement_test.go) · [控制面至双回环 Node](../../internal/nodegateway/controller_poll_test.go)

## 发行验证范围

- 发布包与测试均取自代码提交 `53fefd0`。`go test -count=1 ./...`、`go test -race -count=1 ./...` 和 `go vet ./...` 全部通过。上表 N01–N18 的 41 个具名测试另行逐项复跑，均通过。
- TypeScript SDK 编译、Web 的 TypeScript/Vite 构建、内嵌源码与工作树内容比对。Chromium 打开生产构建，使用模拟管理 API 检查 Worker 预算、覆盖级别、CAS 维护请求和状态刷新。Python 和 TypeScript SDK 使用模拟 HTTP 校验 Worker 查询、维护和撤证的路径、方法、body 及条件头。
- 发布包内的 `sandboxctl` 对接模拟管理 API，验证指定和自动创建请求、候选排除原因的文本与 JSON 输出、操作落点查询，以及维护和撤证的条件更新。
- 在本机启动一个独立 `platformd` 和两个 `sandbox-node` OS 进程，以各自的临时 PKI 和 journal 走真实登记/CSR API。两个 Node 使用同一个本地 Serve 名，均缺少底层 Runtime socket。验证两次登记、相同 CSR 重取同一证书、缺预算拒绝就绪，以及关停其中一个进程后另一个保持原状态。
- 以本地小型合成 Docker-save 归档制作 Linux/amd64 发布包。清单包含三份二进制、Node unit、配置样例等 16 项。安装脚本的只读 `--plan` 核验全部文件，报告 schema 1025、Runtime 1.37.1。
- 待实机验收包括两个独立 Worker 的真实资源准入和同时重启、跨主机 Guest/PTY/服务及桌面、Hibernate 和备份恢复、磁盘预热与大流并发时的心跳延迟。本轮未做生产部署，也未运行双 Worker 压测。
