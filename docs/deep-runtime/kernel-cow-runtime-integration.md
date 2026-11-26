# Worker 内核 CoW 集成候选

独立候选分支为 `feat/kernel-cow-runtime`。当前候选把内核 CoW 接入 libkrun 的 held capture 和 VMM builder，完成了隔离环境里的双 VMM 验证。smolvm 候选固定了 libkrun 提交。Worker 生产请求、CLI 和 WebUI 尚无 kernel CoW 入口，产品 Native Live Fork 仍用既有路径。

| 层 | 固定版本 |
| --- | --- |
| 独立 Linux 测试内核 | `feat/kernel-cow-v1@8d181b563459`，`6.18.65-smolvm-cow-extended` |
| libkrun | `c713851467c74993e9756ba0185fd2ed1393316f` |
| smolvm | `89e258dce55e5ae0b911c9875df1e4a288cefe53` |
| 平台报告基线 | `c603a92` |

## 已运行的验收

按照 Phase 2 内核计划的 K1 和 K2 运行。在同一开发机的可丢弃 L1 内引导测试内核，L2 使用 AMD 嵌套 KVM。外层测试进程只持有访问 KVM 设备所需的组。

| 场景 | 结果与证据 |
| --- | --- |
| 独立内核 KVM 套件 | 6/6。覆盖严格 ABI、活源 writable memslot、只读 anchor、设备线程写入、递归导入，以及 anchor 退出后两个 Guest 首次冷读和独立写。两条结果标记均为 0。日志 `.build/kernel-cow-runtime/k1-kvm-serial.log`。 |
| libkrun 的两个 KVM 进程 | 均使用固定位址和独立 KVM fd。恢复寄存器，验证 `31 → 42` 与子进程 `31 → 53` 的写隔离。源退出后从子 Guest 首次读取另一页。 |
| libkrun held capture 和 builder | 源 Guest 先写 RAM 并经实际 MMIO 完成 Gate 停驻。import 成功前拒绝继续，Native 文件出口拒绝这个点。目标用新 KVM VM 恢复，在源启动 ELF 已移除的条件下，先完成目标身份授权再继续普通 Guest 工作。源退出后子 Guest 冷页读取仍正确。日志 `.build/kernel-cow-runtime/libkrun-held-kvm-serial.log`，其中内核 taint 与 exit 都为 0。 |
| 原有 VMM、Native 和快照回归 | stock 内核上用 `cargo test --locked --offline -p krun-vmm --features blk,net --lib -- --test-threads=2`，176 通过、7 个原有忽略、0 失败。日志 `.build/kernel-cow-runtime/libkrun-native-regression.log`。stock 内核上的 CoW 探测返回 4，提示内核未公布 ABI。 |

复验入口为 libkrun 的 `kernel_cow_kvm` 示例。用同版工具链运行 `cargo build --locked --offline -p krun-vmm --features blk,net --example kernel_cow_kvm --target x86_64-unknown-linux-musl`。在专用 KVM 实验机上按顺序取得 `heavy.lock`、`worker-lab.lock`，将产物和上表对应的 bzImage 传给独立内核仓库的 `scripts/boot-cow-test kvm <bzImage> <kernel_cow_kvm> <serial-log>`。独立内核的 6 项复验使用同仓库 `scripts/run-extended kvm` 与对应 C 探针。二进制和内核摘要见 `.build/kernel-cow-runtime/{k1-inputs,libkrun-held-kvm-inputs}.sha256`。本地候选 gitlink 指向同机对象库中的 libkrun 提交。

## 产品接入剩余门槛

1. Worker 和 resourced 需要通过当前执行身份授权同一次 held cut，交付 pidfd、固定布局与 checkpoint。当前 managed 路径在 smolvm 的 `src/runtime_ext/managed.rs::boot` 中先降权，随后才获取 Native RAM 和装配 libkrun。目标需要在该最终 VMM exec 后、降权前完成 import，然后关闭 pidfd，清除权限，安装最终 sandbox，在同一进程启动新 KVM。用两个实际隔离 UID 验证授权失败、导入成功后的权限清理，以及 seccomp 下的拒绝行为。
2. 用真实 managed 磁盘、卷和网络 Guest 验证相同捕获点的盘切层、活跃设备线程写入隔离、Guest 身份和网络入口。当前 VMM 实验带真实 VMM 设备与 Gate，Guest 是无磁盘与网络的最小程序。
3. 在 resourced 注册可重连的只读 anchor，绑定捕获 ID、执行身份和引用计数，再验同点批次/重试、anchor 丢失、祖先删除、生命周期。持久导出、Hibernate/Restore 和 Standby 要跑原计划 C07–C10。
4. 完成 C11–C12 的产品选择、正式 Worker capability、旧内核回退、Bundle 兼容和三组性能验证之后，再开放 `kernel_cow` 选择。实际合入产品候选还需要记录同一 Kernel/Runtime Bundle 的完整端到端验收。

Linux 补丁仓库、现有 Worker 启动内核和部署版本均维持各自状态。本次测试只启动了隔离 L1，候选提交保存在本地。
