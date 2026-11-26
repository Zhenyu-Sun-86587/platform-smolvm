# 冻结候选 d8cc5d5 的安装与 KVM 验收

2026-11-26 在隔离的 Linux x86_64 Worker 上安装了 Bundle `sha256:c5070578cdf0d4fd01cc120721515c169e9297a3f624803def398996c0e249b5`。从安装目录启动 Serve 和特权 Worker，握手报告了该 Bundle、完整 StatePoint reader 和已测能力。

| 输入 | 固定值 |
| --- | --- |
| smolvm | `d8cc5d5a3585f3570bc071a87eb0de767a3060b7` |
| libkrun | `b77e9d3a62bf3b6d6ba0245cb7b9e6c536772a97` |
| libkrunfw | `0cbb229e7b55698028e78fef9afe9c32797d0b3f` |
| 平台验收工具 | `9eb18ae198be3ab361d1222f7d5c5a8beb70493d` |
| Debian System 盘 | 805306368 字节，`sha256:ad9afc654e73f8640ba5859f2079dc18c66ab4fa73d64c4217338babb1860d7b` |
| 包内测量记录 | `metadata/managed-acceptance.json`，`sha256:9c8856fa12867575d8e3f6b28e7bc4262c42f058fa9f4c3be471281e8ea820c0` |

包内七份 KVM 日志覆盖 OCI 双盘状态捕获、System relay、System Fork、System 连续捕获、两级自动休眠，以及 Native 到 FixedInternal 和反向的私网唤醒。两路网络测量使用相同的测量包 `sha256:f1295d42046808e3eba3abec92cedb4838d7d0314cc88e531a0f02da22df63cc`，各自验证 TCP、Standby、持久休眠恢复和有界 UDP 交付。七份日志逐项重算的摘要均匹配发布记录。

| 安装后验收 | 实测结果 |
| --- | --- |
| OCI 启动 | 构建目录和安装目录各启动两个 OCI Guest。启动证据逐字节相同，SHA-256 为 `bf27ae8b2a0e69feb111b49a71884240851df78a8e423d3718df69515ae52731`。 |
| Serve 和 Worker | 两个实际运行的 ELF 摘要均匹配安装文件。握手确认 System 启动与恢复、Standby 与 Wake，以及 Native、FixedInternal、TAP 和 TAP restore。单列的 Native 身份重建能力仍报告 `unknown`。 |
| 公共 Guest 连接恢复 | 在 Debian Guest 连接存活时强制结束 platformd。重启后仅旧进程的连接保护被释放，独立运行租约保留，延迟到达的旧申请被拒。 |
| 私网休眠切点 | Native 和 FixedInternal 各测试由 Running 或 Standby 开始休眠，在 `STOP_COMMIT` 前和后分别发送获准的真实私网 SYN。提交前撤销，保留原 epoch。提交后两个 SYN 均取得服务回复且共用一次精确恢复，epoch 增加一次。八个场景通过。 |
| 同 Bundle 压力与回收 | 两路 Serve 和一个 Worker 共用隔离 slice。六个并发启动、Fork、Restore 申请在候选全受保护时被拒绝。Access hold 或已应用的常驻策略取消排队回收。放开低优先级 Guest 后，优先级休眠实收 63,315,968 字节。最后一个共享引用退出后，`shmem` 降至 0，`memory.current` 从 113,352,704 降至 1,966,080 字节。 |
| Worker 崩溃恢复 | Worker 经 SIGKILL 后由同一安装件重启。原 VMM 和已应用策略保持，旧 PTY hold 为 `suspect`，旧活动序号无法解除 `unknown`。丢失应答的唤醒与另一请求共用一次精确恢复，重授权绑定新执行，旧 epoch 无法续租。持续压力下观察到完整的 20 秒唤醒宽限。 |

同 Bundle 压力试验在新建 slice 内将 `MemoryMax` 设为 `memory.current` 加 140 MiB，以真实余量触发准入。采样的内存 PSI 为 0，OOM 和 OOM kill 计数为 0。两路 NEW 导出分别预约 580 MiB 和 452 MiB，重叠超额申请被拒，完成或取消后释放预约。第一子机退出时，共同 RAM backing 仍由第二子机和 Worker 持有。第二子机退出后 Worker 释放六个 memfd，存活父机保有相同 inode，`shmem` 保持 86,552,576 字节，直到父机也完成休眠。

## 两个冻结 Bundle 共用 Worker

原始安装目录的 manifest 与产物在试验前后均通过校验。验收在独立 PID、挂载、网络和 cgroup namespace 内启动专用 systemd 管理器。这个管理器的 `smolvmdeep.slice` 同时承载两个 KVM scope，宿主同名 slice 的配置保持原样。夹具将安装目录原样复制到降权 VMM 可访问的路径。两路 Serve 各从独立的状态目录启动，由 `d8cc5d5` Worker 代理资源。

| 实测角色 | Bundle | 运行中 ELF 的 SHA-256 |
| --- | --- | --- |
| `d8cc5d5` Serve | `sha256:c5070578cdf0d4fd01cc120721515c169e9297a3f624803def398996c0e249b5` | `544019a7353cbfafa9862a41887e295599c1a75f5a99e74ddd40832117f107c4` |
| `fdd620e` Serve | `sha256:1c77726df6c3936ce81a2fcb7cbee5787501fc9e078075f2a0e2d17f0008783a` | `5296acc14bbeaf8b40a721e43e864c1bdebca66c725b0ea5304eddb8d28c23fb` |
| 共享 Worker 及重启后的 Worker | `d8cc5d5` | `a4420b24653440ff1698203218773981ec18b80062c403b82fb182cc00476a82` |

`d8cc5d5` 的 reader 为 `libkrun-portable-snapshot-v1:d8cc5d5a3585f3570bc071a87eb0de767a3060b7:b77e9d3a62bf3b6d6ba0245cb7b9e6c536772a97`。`fdd620e` 的 reader 为 `libkrun-portable-snapshot-v1:fdd620ecc2a13ef492dcd310515e18cda9abbefe:b77e9d3a62bf3b6d6ba0245cb7b9e6c536772a97`。两路通过各自的私有 Serve API 创建持久点。交叉恢复均返回 `E_BUNDLE_UNAVAILABLE`，两个待恢复目标的 epoch 保持 0，原持久表示仍可用，Worker 账本无新增分配。共享 Worker 被强制结束并重启后，`fdd620e` reader 恢复原持久点 `sha256:de1672e6fdc8a966bcb9924297007f2f9dd0a4f512cb824f02f008b7a35a0d31`。原 Guest PID 433 从第 36 次计数继续，两块盘的标志值分别为 35 和 209934011897941，与源在该边界一致。

六个并发启动、Fork、Restore 申请均被拒，受保护的两个 VMM 继续运行。仅低优先级的旧 Bundle Guest 完成自动休眠，`memory.current` 从 129,335,296 降至 66,400,256 字节，实收 62,935,040 字节。触发时 slice 可用余量 146,788,352 字节，内存 PSI 为 0，OOM 和 OOM kill 均为 0。第二子机退出后，Worker 释放七个共同 memfd，父机仍持有。父机完成休眠后 `shmem` 从 76,025,856 降至 0，整个共享引用阶段 `memory.current` 下降 101,601,280 字节。本组使用夹具上报活动，并采集真实 cgroup 数据。

复跑夹具位于 smolvm 提交 `e03c1ec9c3b19806561bcf566a5e9c7fa69f3459`。入口是 `tests/run_frozen_worker_pressure_in_systemd.sh`，在其隔离环境中执行 `tests/test_deep_worker_pressure.py --bundle <d8cc5d5-installed> --peer-bundle <fdd620e-installed> --guest <accept-block-counter>`。外层 runner 使用 systemd 255 委派专用 cgroup，测试完成后清理该委派。

工作区记录位于 `.build/release-d8cc5d5/`。

| 记录 | SHA-256 |
| --- | --- |
| `out/installed-handshake.json` | `7a9364d07c6ce7e558b122ca543ef03acb97f2f9fde70bea099ec4457e51ec45` |
| `installed-connection-kvm.log` | `855d86a000d2e53681bd7f38b5f7eaa62cee952edf99d430190a7fcfb61ab126` |
| `installed-hibernate-cut-kvm.log` | `750aaed8bf745955984e850947aa42f069373d22e33a8b6c2c538b473f74ab8e` |
| `installed-worker-pressure-kvm.log` | `1de0a52992619bb2c439bb21ec8019876a16fe81122ea8a6ecbd05eff1b0e8f1` |
| `installed-dual-bundle-kvm-v3.log` | `ded8eeb0fc167e293c468896fc6b0241a3e295af000b87d8a5959871a41ebdbf` |

各工作包余下的组合验收见[完整台账](acceptance-ledger.md)。
