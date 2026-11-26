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

## 同一批次内取消、重试与父机删除

公开 Fork Batch 在已安装的 `86dae32` 上接受运行中取消。一个子机已经提交时，操作保留了该子机及其独立资源，只终止仍待发布的目标。平台重启后，原创建键与取消键均返回已记录的批次；源机和成功子机分别从根盘读回了捕获的标记。记录为 `.build/wp23/public-fork-cancel-kvm.log`。

使用开发 Worker `1f7a35c` 的同一批次实验先启动真实持久任务，关闭普通 Guest Agent，随后让三个子机分别进入已提交、待发布和已失败状态。公开取消保留前一个成功子和原失败原因。父机经 Stop、Delete 确认退出并回收私有盘后，公开 retry 只重试失败子。重试保留 capture 与三个机器 ID，已取消的目标持续受围栏。新目标随后在自己的任务命名空间认领原 run，其 PID 627 返回捕获前的随机标记和子机收到的回复。夹具为 `tests/integration/managed_system_vsock.py --case task-fork-cancel-retry`，记录为 `.build/wp23/public-fork-cancel-retry-delete-kvm-v2.log`。

Chromium 使用常规 Tasks 页，在原本没有 `/workspace` 的 Debian 镜像上从表单启动任务。页面显示了真实工作目录和随机输出。取消该任务时，输出窗口一直读到末尾。记录为 `.build/wp23/public-task-ui-kvm-v2.log`。

## StatePoint 引用与实物回收

2026-11-26 在隔离 x86_64 KVM Worker 上用真实 Debian 根盘运行。公开 VM checkpoint 与随后 Hibernate 共用一块 qcow2 代际。Worker 同时保留两个 StatePoint 根、原生备份导出和未完成的操作引用。公开 checkpoint 删除的同键重放返回原操作。删除期间并发 GC 被拒，已获得的导出仍经 Unix 文件描述符读取同一 inode。第一次导出释放后，休眠点经公开 `start` 恢复到 epoch 2，Guest 读回原 tmpfs 和根盘的同一随机标记。

还能恢复休眠机时，Worker 拒绝释放该休眠根。成功恢复、Stop 和 Delete 后才释放旧休眠根。未决操作与独立块导出依次单独持有同一代际，各自都阻止回收。最后导出释放后，夹具在精确块文件的 unlink 前暂停 Worker。磁盘仍为原 inode，目录中保留 `deleting` 修订与出边。用 pidfd 终止 Worker 后，同一二进制以新会话启动。恢复围栏生效时显式 `CollectResource` 被拒。围栏解除后，自动回收确认文件已移除再提交 `deleted`，清除依赖边并保留导入模板原字节。

最终开发 Bundle 为 `sha256:fce5eef7a49ffdf7256c193963dd261f743a0e27e12526f95b9c94c1b699d3d5`，reader 为 `development-resource-gc-v2`，Worker 为 `sha256:2458d27a6e430500522704263a9cc753802b38c2eaf8a0f34c18b0450fd572da`。该次共享块 inode 318792 实占 2,760,704 字节，并发围栏拒绝 60 次。删除 checkpoint 回收了 55 个只被它引用的 chunk，休眠根继续保留 57 个共享 chunk，直至旧休眠根释放。Worker 将原 tombstone 的修订 3 推进到删除后的修订 4。原始记录在 `.build/release-d8cc5d5/worker-gc-a05-kvm-v6.log`（SHA-256 `f8f42ecd16d589c0e050facbac106f9994d423d1168db7d44382d2300ddfbc05`）。

另一次完整复测在资源回收后从平台配置移除原运行时并重启 platformd。同凭据、幂等键与修订返回原先成功的删除操作。复用该键修改修订得到 `IDEMPOTENCY_CONFLICT`，新键提交则得到 `NOT_SUPPORTED`。平台先核对回执，再为新操作检查原 Worker 路由。记录为 `.build/wp23/a05-route-retirement-kvm-v2.log`（SHA-256 `bca24be727c8e22fbf0789eb74be207b87ad1b5597007f85e35c3301c00eda3d`）。

后续复测使用 `f2df146` 的冻结构建和独立 System 测量 Bundle `sha256:1d79bf8139035f924f8132fe58837686d6da9a0c72bc2c3ee2f32829882bedff`。原 Worker 在正常导出时拒绝了该 Bundle 的 reader，见 `.build/wp23/a05-f2df146-measurement-v1.log`。reader 以冒号分隔合约与修订，导出端和离线读取端误用了文件对象 ID 的格式约束。smolvm `f5aef70` 修复这两处校验，继续核对指定 reader、Bundle 和归档摘要。

保持原 Serve、VMM、平台和 Debian 镜像，仅以开发 Worker `d81a5b2f5ad8d322bacc641a1a330b54db77085bd691d2bf317e0cac91c515cd` 复测。60 次并发 GC 都受引用保护，删除 checkpoint 释放 56 个私有 chunk，休眠根继续保护 55 个共同 chunk。恢复到 epoch 2 后，原 Guest 在内存和根盘读到同一标记。最后引用释放时终止 Worker，重启后清除原 inode，修订从 3 到 4。原始记录 `.build/wp23/a05-f2df146-worker-reader-v2.log` 的 SHA-256 为 `530d1d211965908fb7d7c939f231ee6dd2af87ace0c1319c8e43b42b06e61027`。该次 `f2df146` 的独立安装只公布 OCI 能力，完整 System 安装复测随包含该修复的发行包进行。

复现使用开发 Worker 与匹配的 platformd。独立网络和挂载命名空间、临时存储目录及专用 systemd slice 由夹具创建。测试库只装入此次 Worker，依据块文件的精确路径设置崩溃切点。

```sh
cc -Wall -Wextra -Werror -shared -fPIC tests/fixtures/worker-gc-cut.c -o "$CUT_LIBRARY" -ldl
sudo env PYTHONDONTWRITEBYTECODE=1 python3 -B tests/integration/worker_gc_kvm.py \
  --bundle "$BUNDLE" --platformd "$PLATFORMD" --system-image "$SYSTEM_IMAGE" \
  --cut-library "$CUT_LIBRARY"
```

## 组件重启时的网络和卷写入

在同一份 `fb69434` 冻结安装和配套 platformd 上启动两台 Debian，一台使用 Native，另一台使用 FixedInternal。发送端建立一次 TCP 连接。每次中断后，测试通过原 VMM 的 Guest Agent 要求发送端在该连接上发送新随机数。接收端用已打开的独立卷写入并同步随机数，随后应答。测试从 Guest 块设备单独读回整段日志，逐次比较两端的进程、套接字 inode、TCP 地址和端口，并从宿主核对 VMM PID、启动时间、cgroup 和 writer 的 OFD 锁。

platformd 和 Serve 的进程分别退出、重启，四个时点均成功读回应答和完整卷日志。另两次试验在 systemd 接管新 VMM 之前，以及接管成功而 Serve 尚未发放启动许可时，终止本次 Serve。尚未获得许可的两个进程均退出，Worker 没有为它们登记执行或发放 writer。原公开操作在新 Serve 上完成，各启动一台新 VMM。原有两台 Guest 在这两个切点继续使用相同 TCP 会话写入独立卷。

broker 被终止后，原连接又交付一次随机数，两台 Guest 的 writer 与平台绑定仍在。重新启动同一 broker 后，它确认两台原 VMM 及三个实际 writer 文件，但 Native 与 FixedInternal 两条原 TAP 租约均保持 `unknown`。`CompleteRecovery` 返回 `E_RECOVERING`。下一条挑战在七秒内没有应答，接收端也没有写入该随机数。原安装的网络恢复仍待修复。在该次试验中两端 socket inode 分别为 881 和 1005，接收端在中断前写入并核验了 672 字节。

复现使用 `component_restart_kvm.py --scope-cuts --expect-unrecovered-network`，另传同批 `--bundle`、`--platformd` 和 `--system-image`。正向模式省略 `--expect-unrecovered-network`，须由新 broker 自动恢复到 ready，并在原 TCP 会话上继续写盘。实测记录 `.build/wp23/a20-component-stream-fb69434-v5.log` 的 SHA-256 为 `871cd7f86a2c58dfaaeb00d57d56380cac6e7dacc858e03d3843abd395079e0e`。[组件重启夹具](../../tests/integration/component_restart_kvm.py)

加入 TAP 恢复回执的 Worker 在同一套测试中重新确认了两个存活 VMM 的 TAP、网络命名空间和策略。两个 systemd 接管切点及三个组件分别重启后，原连接累计完成九轮请求，接收端的原卷包含完整的 756 字节记录。独立测试还确认两端原 DNS、服务拨号和最终 Stop 后的租约释放。日志 `.build/wp23/a20-stream-scope-network-v2b.log` 和 `.build/wp23/a20-network-dns-dial-v3.log` 的 SHA-256 分别为 `04a40a30f4c76465ea5f1173a8c846a6f57a1be4caf83c7588cb6638d59e861e`、`d36da840dc5abfc35cd053ae5fcf0a638f7031bd9b3577b548e5c3586966a743`。[网络恢复与复现输入](A20_LIVE_NETWORK_RECOVERY.md)

## Worker 主机换 boot 后的恢复

夹具两次启动同一块 Ubuntu 24.04 虚拟系统盘。第一次启动在嵌套 KVM 中运行三台 Debian。一台在 tmpfs 和根盘各写入同一随机数，调用公开 Hibernate 并记录保存点摘要。另外两台为正在运行的源和 Fork 子机，两份已授予的写权限以及子机正在读取的易失 RAM 代际均可在 Worker 核对。宿主文件系统同步后，一组由 Guest systemd 正常关机，另一组直接终止本次 QEMU。第二次启动换了 boot ID，复用原 ext4 状态盘和只读安装输入，运行目录与套接字重新创建。这两组实测的断电边界为虚拟 Worker 的电源。

在旧 `fb69434` 中，换 boot 后 Worker 保持 `recovering`，需要逐一确认两个旧执行结束。旧平台在超过观测时限后将保留的休眠点显示为 `unknown`。记录为 `.build/wp23/a20-worker-host-fb69434-v4.log`，SHA-256 `e8bb042289501ed7f125934d84e457b656330e5d558b0bc58a2a975eda2449a9`。

修复后的 Worker 核对旧 boot ID 及实际写盘文件后，自动释放两份旧授权。原易失 RAM 代际进入 `lost`。新平台向 Worker 确认休眠点、空执行槽位和 epoch，公开详情恢复为 `hibernated`。从该页调用 Start 后，两个场景都在 epoch 2 读回原 tmpfs 与根盘的随机数。开发 Bundle 为 `sha256:5d110dcb5866da3c941ce29dfbbf32c0b38e69642084975aa0f11480d61bc7cb`，Worker 为 `ce61b8ed81415201d0d67b6990a134ba5725045a0867517731b606da170380de`，platformd 为 `4abf8a0fe0e7fa109aec8ab14323eb590d73f83e074930eb8e7d9438fafa3373`。VMM、libkrun 和 Debian 镜像来自 `fb69434`。

| 第二次启动前的动作 | 记录 | SHA-256 |
| --- | --- | --- |
| 虚拟 Worker 断电 | `.build/wp23/a20-worker-host-auto-v1.log` | `bf6a5e5b295be7dd9592235226c8257eec713091b063e319de02aedd567a20b6` |
| Guest systemd 正常关机 | `.build/wp23/a20-worker-host-orderly-v1.log` | `a22b1869201c1c883432beefb1bf626c1f1eaec7f1f1bcd29b0b525279ee2066` |

另一次断电使用 `f2df146` Worker、修复时钟的 `0d02553` libkrun 与 `83b3d29` platformd。新 boot 在旧授权的实际盘 inode 上持有一个独立的排他锁，Worker 保留原写入预订和 `unknown` 授权，并持续报告 `recovering`。释放该锁后，Worker 自动完成围栏。Guest 在原计时进程中经过至少 26 秒 managed Standby、Hibernate 和整机重启，首次采样时原定时器仍未到期。Guest 的 monotonic、boottime 和 realtime 均在 18.069 秒时达到原定的 18 秒期限。该测试使用重新测量的开发 Bundle `sha256:6e112dd4c836df1ad2b87b5eb42d1f17ee7738262bdd19ec19bbee66b1f77c75`、旧安装的 VMM 与 firmware，宿主基础镜像分配 1792 MiB。日志 `.build/wp23/a08-a20-worker-host-f2df146-clock-ofd-v4.log` 的 SHA-256 为 `6480aa68d8f5ac4b481f7b9cd675417003d06e3c6e04dcf588635310973a7ac6`。

复跑时传入本机构建的可嵌套 KVM 的 Worker 基础镜像、宿主内核与对应模块、平台程序、Bundle 安装目录和 Debian 系统镜像。使用 `--automatic-recovery` 验证全自动恢复，增加 `--orderly-shutdown` 可切换关机方式。[Worker 整机夹具](../../tests/integration/worker_host_restart_kvm.py)

### 正式安装的整机复测

两组整机复测使用 `1eb5104` 正式安装 `sha256:b00c0f05bc54d69bbef854ea16c3cac42d36ddc9f8413a1cedd5e5e290434f97` 和锁定平台 `c095c4f`。每组在 2304 MiB 的独立 Ubuntu Worker 宿主中，生成一个 Debian 保存点并保留一对有活跃 writer 的源机和 Fork 子机。第二次 boot 由当前 owner 重新登录并通过公开操作恢复保存点。[复跑夹具 `886d724`](../../tests/integration/worker_host_restart_kvm.py)

强制断电组的 boot ID 从 `63d72f36-bfab-46cc-b84e-26685680cf48` 变为 `a6b145fe-4cfd-40d8-96b9-5e0b81f73009`。新 boot 在原盘 inode 上持有独立排他锁期间，Worker 保留旧 writer 预订。释放后自动完成两个旧执行的围栏，回收其写授权，原易失代际记为丢失。原保存点的新执行进入 epoch 2，读回原 RAM 与盘标记。定时进程 PID 753 在恢复后首次取样仍未到期，三个 Guest 时钟均约 `18.046` 秒到期。`.build/wp23/a20-power-final-1eb5104-v1.log` 的 SHA-256 为 `719d715c3cbabf2091f0ad60c440bc740c3f124da29131eda7573b0c1cfeb684`。

正常关机组由原宿主的 systemd 完成关机，QEMU 正常退出后从同一盘启动。boot ID 从 `5a469bc0-4f0f-4654-8379-5bf32ffda3b9` 变为 `47dbd34d-f60c-43e7-99b6-0391be387c8c`，两个旧执行自动围栏和释放写授权，一个易失代际标为丢失。对原保存点的公开 Start 在 epoch 2 读回 RAM 和根盘标记 `eed731516721f4260a1d6c32c2d53b85`。`.build/wp23/a20-orderly-final-1eb5104-v1.log` 的 SHA-256 为 `5d1aae2f2724c9cd21a5a1d6fedeb223058715874cacdb53c25abb42df4ac122`。
