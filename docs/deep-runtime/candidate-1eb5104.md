# 冻结候选 1eb5104 的授权发行验收

2026-11-26 在隔离的 Linux x86_64 KVM 宿主机上原子安装 Bundle `sha256:b00c0f05bc54d69bbef854ea16c3cac42d36ddc9f8413a1cedd5e5e290434f97`。安装目录为 `/var/tmp/deepfork-smolvm-build-cache/release-1eb5104/installed`，构建和实测证据在 `.build/release-1eb5104/`。

## 冻结输入

| 输入 | 固定值 |
| --- | --- |
| smolvm | `1eb5104630b01fe5e21494f979579f3fa9924427` |
| libkrun | `0d0255383f4378aca5be7bf19629330c08fcaf8e` |
| libkrunfw | `4d5852a59f9c8bc75780544d0fe93b5015343a32` |
| 平台及验收夹具 | `c095c4f625c85000bee6de656c582a63ff0e8db7` |
| platformd SHA-256 | `c7045e98f56f09805774ea0c785f7773a172831042e88bd13dca6168875659ba` |
| sandboxctl SHA-256 | `c54a8904977033d6dc18be7a4abbc93bc1551b839af4febcb54fe9751f5a0609` |
| Linux 6.18.60 源归档 SHA-256 | `3c2f44ef6acc021a7ad444d9a2e6ec1ef488ff09368793d0004a6080f69d621e` |
| 运行时构建锁 SHA-256 | `316aa16ad6ebc672a7c1e59428ad1225d7eb11b17e7145a2bdd8ec7662f38066` |
| 工具链锁 SHA-256 | `32943b623419022f5fba0908602962791d5aa5a214a67503eec9c8f85992433f` |
| 库阶段构建记录 SHA-256 | `25e8293660c711ff9254954684f5c7551b9ce8c08fa25683a19acc7b0b0bc748` |
| 运行时构建记录 SHA-256 | `6a24bb9426f75f9374248e203733b13458cdd2364e9007097dcef3495bcdd05e` |
| Serve SHA-256 | `a9d506cab7a753c0608d87f9bfbaf5b6b2eb49789973c327d4a8b42fcf711186` |
| Worker SHA-256 | `f9a0812af46952abdd86cb99f596862e8f9f097c3e84dbebbe92cac50ad23448` |
| Debian System 盘 | 805306368 字节，SHA-256 `f7b0611ebb21af630addeddf95717aae284f7aa44b4abe2ee7c3933919570b02` |
| Guest Agent SHA-256 | `a10f70ce0be905e0a3b133decac0bb3f32e8de0299c42c171e1a9fd26c4b5f66` |
| Guest identity-helper SHA-256 | `424f7836ee4648a0d00399a5103daea3ff8bdf45d2508e8717d638bfcfbbc8ee` |

内核使用 `deep-x86_64.config` 和 `managed-identity-x86_64.config` 两片配置。运行时沿用原发行优化配置，在私有 mount namespace 内使用单个 Cargo job。经验证的库阶段保留在 ext4。原始锁、构建日志及启动时内存记录位于 `.build/release-1eb5104/`。先前 `1451bee` 构建的两次宿主内存不足记录和锁保存在 `.build/release-1451bee/`。

Serve、Worker 和 libkrun 的最高 glibc 符号版本均为 2.30。Debian 盘在只读文件系统检查通过后，重新读取盘内 Agent 与 identity-helper，两者的 SHA-256 均与最终构建的 Guest 文件一致。检查见 `system-inputs.log`。

## 八项包内测量

锁定的 `measure-managed.py --authorized-only --authorized-lifecycle --authorized-owner-case system-tasks-delete --authorized-owner-case system-rollback` 从同组运行时字节和 Debian 盘构建测量 Bundle `sha256:d6c7d47aab3ff412f51137b5eaed2b8f3f6b50d486684d84dc691f99d694f06b`。每组使用独立 Worker 目录、私有挂载和网络命名空间。`RuntimeManagedAcceptance/4` 的 SHA-256 为 `e9824da92c9460b4c78a03a91916268a50949c4db8adb21fd6314c96df86f6ef`。原始测量日志在安装树的 `metadata/managed/` 和宿主工作区的 `authorized-attempt.xtL970dJ/managed/`。

| 用例 | 实测范围 | 原始日志 SHA-256 |
| --- | --- | --- |
| `system-boot` | Debian `systemd` 为 PID 1，正常 Guest 入口读回自有机的 RAM 标记。 | `85a4c83f63d829bde80def4c50164b81cc3f6cdf3c8958862da43ac937c83326` |
| `system-fork` | 公开批次产生两个独立子执行，子机和继续运行的源机均读回相同 RAM 标记。 | `88663a9dc6f8f1942c20a9a761a6239c05d7bfdceac937acec32e787b900fd79` |
| `system-restore` | 公开 Checkpoint 生成独立恢复，随后同一源机 Hibernate 和 Start 使用原 capture，恢复 RAM 并推进 epoch。 | `05f3e7e9fc39f05bcc0aae0cdf0736b1fe76f822a353ba74079ea1da220fb8ae` |
| `network-native` | 自有 FixedInternal 源经 TCP 唤醒休眠的 Native 目标。两个业务请求仅产生一次 exact 恢复，保留原地址和挂接。 | `ee2b2e32583e0383978ddf90da12b77ce926fe0b55bb99e8f30e373832a2c4fa` |
| `network-fixed` | 自有 Native 源对 FixedInternal 目标验证相同的 TCP、原 capture、地址和业务回执。 | `6ca52a76b4828987fcf50b3ef7438f5c253fdae8c56b05bf7595d2407ab6300b` |
| `system-lifecycle` | 所有者保护活动，释放后 Standby 和 Wake 保持原执行。手动 Stop 退出该执行，随后冷启动进入下一 epoch。 | `7d4bffe4d1545519100397ecceb4173119bc9c399ecbec1c709a4406c8fe0c67` |
| `system-tasks-delete` | 休眠机启动所有者任务并推进 epoch，同键返回同一操作。取消任务后 Stop、Delete，记录执行及删除回执。 | `990b6445daffa31b49b95bfcb2af4952b6a770e6a44eabe4f8e772f5e03ac927` |
| `system-rollback` | 同机 Rollback 让原 VMM 退出，保留单独 rescue capture，并在新 epoch 恢复原 RAM 和根盘标记。 | `0ad36547c75646ba9c35174e556a3666f3f9018b1195a79addd408b37a7bc097` |

## 安装及公布能力

| 验收 | 结果 |
| --- | --- |
| 原子安装 | 打包与原子安装各记录同一摘要。以期待的 SHA-256 独立运行安装验证再次通过。`authorized-attempt.xtL970dJ/{pack,install}.log` 记录打包和发布。 |
| OCI 启动 | 打包前和安装后各启动两个真实 OCI Guest。两份 `RuntimeOciBootEvidence/1` 逐字节相同，SHA-256 `582877c1b88064c8ace0d5699d96bdcc3c0e4975467ede899d219f1fe1210e4b`。 |
| 运行进程身份 | 使用安装 Wrapper 启动 Serve，使用同目录二进制启动 root Worker。`/proc/<pid>/exe`、Worker socket `SO_PEERCRED`、Worker Health 和握手均对应本安装。`out/installed-handshake.json` SHA-256 `c6f9376316adb5fb7b698c2790d88ef21d2c31b4cce620efef6a2b8b907bed7e`。 |
| Serve 直接握手 | System 启动与恢复为 `system-debian-v1`，Checkpoint、Hibernate、Restore 为 `managed-block-v1`，独立 Fork 和 Fork 批次为 `managed-parked-v1`，两种私网模式、TAP 和 TAP restore 为 `managed-private-system-v1`。 |
| 平台所有者能力 | 平台适配器从同一安装清单核对 `authorized_lifecycle` 和 `authorized_owner` 后公布相应的生命周期策略、Standby、Wake、Stop、任务启动、Delete 与 Rollback。实测范围见八项测量和各安装后用例。 |
| 时钟及其他握手字段 | 时钟档案为 `RuntimeClockContract/2`，状态格式 `SMVCSBY1`。`network.native_reidentity` 与 `boot.live_compute_growth` 在 Serve 握手中为 `unknown`。 |

安装后的 System 归档和测量原 ext4 以同一 SHA-256 验证。`release-receipt.json` 记录安装摘要、三份源码归档内容、System 档案和运行进程的摘要。

## 安装后公开验收

用 `authorized_services_kvm.py --case system-restore --use-installation` 只读绑定正式安装树。公开 Checkpoint `snap_899c9aba19cb73a375a7e3576b865b50` 派生了不同逻辑实例上的独立恢复。源机 Hibernate 后以 capture `point-f4ccc6335ff02e25660726d68a0b6ff0` 精确唤醒，在原逻辑实例上从 epoch 1 进入 epoch 2。两个执行都读回 `ram-c5e332549ff7005476523e23`。测试后的安装摘要再次验证通过。原始日志 `installed-public-system-statepoint.log` 的 SHA-256 为 `8427962a92328445985600ca1407ec08c34c36de62cb4324a2d46b3bc2690ffa`。

### A11、A12 卷与块设备

`volume_product_kvm.py` 以 `--use-installation --state-tmpfs` 从同一安装、平台和 System 原盘启动真实虚机。各组使用私有暂存和专属 Worker slice。

| 用例 | 实测结果 | 原始日志与 SHA-256 |
| --- | --- | --- |
| A11 `deleted-checkpoint --verify-retired-head --machine-volumes` | 源两卷分别推进后转挂并读回新写入。删除源机后回收当前根盘写头，保存点根盘仍有引用。`exact` 返回 `VOLUME_GENERATION_MISMATCH` 且 effect 为 `none`。`clone_conflicts` 恢复原 RAM、两份原卷字节和新建的公开 RW 卷，共享同一不可变 RO 代际。重启平台后核对卷投影与写头。Hibernate 和 exact Wake 保留卷 ID 与后续写入。 | `installed-a11-volume-projection.log`，`0583f4245115a872eaca2d193e165d7f8edf1001916b6949ce501a2f0ef1468b` |
| A12 `lifecycle --machine-volumes` | 在线挂接、解挂和扩容保留外层错误码并给出 `reason_code=REQUIRES_STOPPED`。Stop 后持久卷转挂成功，块设备从 16 扩至 32 MiB。一次 Guest 文件系统扩容因只读失败，随后正常扩容并读回原文件。删除源机回收私有根盘和此前解挂的 machine 写头，历史 ephemeral 卷仍可列出。两个自有 RO 消费者可继续挂载和读取同一不可变代际。 | `installed-a12-lifecycle.log`，`90f5ed7e0d5f6928eedd2416b067f5cf58b994a15aabde3d2f3549b445843536` |
| A12 `machine-fork --machine-volumes` | 独立 Fork 给 machine 和 persistent 两个 RW 卷各创建新公开卷，并引用同一不可变共享代际。源机删除回收其私有写头。子机在 epoch 2 和 3 冷启动时保留原根盘写头和子机数据。子机删除回收其私有头，两份持久卷和共享代际仍可用。 | `installed-a12-machine-fork.log`，`5bb850aed65598ff9cfed6106981a1daeaf1371c2d37f58b1410be050f462bb2` |
| A12 `native-roles` | 真实 System 从同一 capture 恢复 `root`、`storage`、`overlay`、`cache` 四个 RW 角色及 `data` RO 角色。读取各捕获标记和原 RAM。目标的四个写头均独立，RO 代际保持原标识。 | `installed-a12-native-roles.log`，`3506bb4fad29c48dac4b29baa7de318080a52c8d827a26b6d592c9364dcbe492` |
| A12 `growth-fault` | 分盘后 objects ext4 剩余 0 字节，目录数据库盘仍有 3,886,411,776 字节且 Worker 就绪。ENOSPC 使创建和扩容在发布前中止，原 inode 15 和原 16 MiB 数据保留。原写头进入 `0400` 后的受控 fsync EIO 留下待调和状态。Worker 重启后通过公开 reconcile 复用后继 inode 19，实机以 32 MiB 设备读回原数据并写入新数据。 | `installed-a12-storage-isolated.log`，`25a5af2c356c0c4a8bf0736caec722fac9bb915a49735396077e46222c0277e0` |

首次存储切点用了旧夹具的挂载顺序。catalog 的 bind 覆盖了预先挂上的对象盘，导致 objects 与 SQLite WAL 实际处于同一已填满的文件系统。两个原始复跑出现 `ApplyProductVolume: empty request packet`。只读观察对照中，Worker 先为 `ready`，随后退出码为 101，两个目录同时剩余 0 字节且无新操作记录。日志为 `installed-a12-storage-cut.log`、`installed-a12-storage-cut-v2.log` 和 `installed-a12-storage-observed.log`。夹具提交 `fccd03d2e407d17689b27f6b6ee7b3f02f84595e` 将真实 ext4 挂在 catalog bind 之后并断言设备独立。表中通过记录使用原正式安装及平台二进制和修正后的测试夹具。

另一次目录空间对照将 Worker 日志写到独立文件系统，记录 SQLite 的 `database or disk is full`。Worker 保持 `ready`，公开操作留在 `recovery_required`。日志和 SQLite 共盘时，调用跟踪捕获 stderr 的 `ENOSPC`、Rust 标准库 `stdio.rs:1209:9` 的日志写入 panic 和退出 101。跟踪文件 `installed-a12-catalog-full-worker.strace` 的 SHA-256 为 `3fc8b94c6037b9ac73de59bbfe3adfad912cce0755e05e5c4f66527782a4347c`。

Worker 修复 `bb9fb77b0cc942c71eb408aa99047e2b8e506d96` 保留 SQLite 的类型化错误，并将诊断日志写失败按尽力写入处理。独立小目录复测中，两个 Worker 都先登记真实安装 Bundle，再接收普通目录操作。原安装的 Worker 退出 101。修复后的开发 Worker 保持同一 PID 并返回 `E_CATALOG`，释放空间后同一操作 ID 正常受理。原件对照日志 `catalog-logging-frozen-control-registered.log` 的 SHA-256 为 `09f75ea6fe575f420fff2344f2e9305b887518e3e12efa5eb34b24589b941c78`，开发复测 `catalog-logging-repaired.log` 的 SHA-256 为 `6f2bf099e65fc716ddd0c02040bc41b751fa0ea92b0984add70e420113edc711`。

### A17 OCI 管理 Agent 重启

`oci_agent_restart_kvm.py` 只读绑定正式安装并启动官方 OCI 根文件系统。管理 Agent 被替换后，夹具核对原 crun 进程、启动时间、工作负载标记和持续计数。

| 用例 | 实测结果 | 原始日志与 SHA-256 |
| --- | --- | --- |
| `--agent-signal TERM --end manual` | 新 Agent 接回 crun PID 487，stdout 计数继续至 36。显式 Stop 给原进程发送已登记的 SIGUSR1，原 VMM 退出。 | `installed-a17-oci-agent-term.log`，`e4d6b0a5f13fcc782cc9f0b86b20279141847e527016bdad24eafd92e0b4d06f` |
| `--agent-signal KILL --end workload` | 新 Agent 接回 crun PID 488，stdout 计数继续至 32。原任务自行退出后，stop-on-exit 关闭同一 VMM。 | `installed-a17-oci-agent-kill.log`，`d98962a3223855d80a116d277f67a55c50d8d666e854f4255fde8dc95d5dc462` |
| `--agent-signal KILL --end branch` | 独立 OCI 分支及其替换的 Agent 都读取同一 tmpfs 工作负载登记，并接回原 crun PID 481。 | `installed-a17-oci-agent-branch.log`，`15e36731b781e646812c955eb0780a661ea76500fcbcc01ea54b4d4cbcee8b32` |

[授权 System 夹具](../../tests/integration/authorized_services_kvm.py) · [实际卷夹具](../../tests/integration/volume_product_kvm.py) · [OCI Agent 夹具](../../tests/integration/oci_agent_restart_kvm.py) · [f2df146 授权验证记录](candidate-f2df146-authorized.md) · [逐项验收台账](acceptance-ledger.md)
