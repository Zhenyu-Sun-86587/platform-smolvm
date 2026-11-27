# Worker 内核 CoW 集成候选

独立候选分支为 `feat/kernel-cow-runtime`。11 月 27 日的候选把 CoW RAM 接入可选的 Worker 启动与 Fork 合同，并保留原有默认行为。下文的初始候选及历史验收均标明对应版本。本次新增的源码、启动包和实测范围在文末登记。

## 跨 UID 启动候选

Linux 测试配置 `f68cfa91e953` 构建了 `6.18.65-smolvm-cow-worker`，新增 seccomp 过滤。libkrun `c13c9c5e13a8` 提供 Worker 私有入口，在同一目标进程里用持有的源 pidfd 导入，关闭 pidfd、清除能力并永久切换 UID/GID，然后由正常 VMM 构建读取已导入的内存。源侧的私有控制出口只传绑定到当前暂停会话的区域列表与 checkpoint，源在导入确认前保持 held。smolvm `e2b6066ddf63` 固定这个 libkrun 候选。

使用真实 KVM 的跨 UID 用例通过。源 UID 为 20101，普通目标 UID 为 20102。普通目标的导入受内核权限拒绝，授权启动阶段导入成功，无效源描述符也会完成降权和关闭描述符。目标的有效、许可、可继承和 ambient 能力全部清零。之后 seccomp 拒绝私有 CoW syscall，目标仍能创建 KVM、完成身份激活和运行。源退出后首次冷读与写隔离通过，内核 taint 和测试退出码均为 0。日志在 `.build/kernel-cow-worker/kvm-worker-serial.log`，镜像与用例哈希在同目录的 `kvm-worker-inputs.sha256`。私有 ABI 已通过共享库构建、符号核对和错误输入描述符回收测试，记录在 `runtime-final-build.log`。

复验使用 libkrun 的 `src/vmm/examples/boot_kernel_cow_worker.sh <bzImage> <kernel_cow_kvm> <serial-log>`。在专用 KVM 环境依次取得 `heavy.lock` 与 `worker-lab.lock`。测试 bundle 为 `.build/kernel-cow-worker/smolvm-kernel-worker-lab.tar.gz`，包内各项 SHA-256 已解包核对。

同一个 `worker` 内核还通过了独立 KVM 套件 6/6，记录在 `k1-kvm-worker-serial.log`。常规内核上的候选回归为 VMM 176 项通过、7 项忽略，libkrun 27 项通过，记录在 `native-regression.log`。各日志路径相对于上面的 `.build/kernel-cow-worker`。

## 初始 held VMM 候选

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
| 独立内核 MM 套件 | 23/23。覆盖 C01–C04 和 C06 的权限、地址、ENOMEM/信号清理、THP、swap、回收，以及 NUMA 迁移。内核 taint 与 exit 均为 0。日志 `.build/kernel-cow-runtime/k1-mm-serial.log`。 |
| 独立内核 KVM 套件 | 6/6。覆盖严格 ABI、活源 writable memslot、只读 anchor、设备线程写入、递归导入，以及 anchor 退出后两个 Guest 首次冷读和独立写。两条结果标记均为 0。日志 `.build/kernel-cow-runtime/k1-kvm-serial.log`。 |
| libkrun 的两个 KVM 进程 | 均使用固定位址和独立 KVM fd。恢复寄存器，验证 `31 → 42` 与子进程 `31 → 53` 的写隔离。源退出后从子 Guest 首次读取另一页。 |
| libkrun held capture 和 builder | 源 Guest 先写 RAM 并经实际 MMIO 完成 Gate 停驻。import 成功前拒绝继续，Native 文件出口拒绝这个点。目标用新 KVM VM 恢复，在源启动 ELF 已移除的条件下，先完成目标身份授权再继续普通 Guest 工作。源退出后子 Guest 冷页读取仍正确。日志 `.build/kernel-cow-runtime/libkrun-held-kvm-serial.log`，其中内核 taint 与 exit 都为 0。 |
| 原有 VMM、Native 和快照回归 | stock 内核上用 `cargo test --locked --offline -p krun-vmm --features blk,net --lib -- --test-threads=2`，176 通过、7 个原有忽略、0 失败。日志 `.build/kernel-cow-runtime/libkrun-native-regression.log`。stock 内核上的 CoW 探测返回 4，提示内核未公布 ABI。 |
| libkrun 动态库 | `make debug BLK=1 NET=1` 完成。产物 `libkrun.so.2.0.0`，SHA-256 为 `1c3d5035142cf40719cdd34ac2afd8441d30d3d06bb8f5e1407e375d273acd13`。日志 `.build/kernel-cow-runtime/libkrun-shared-build.log`。 |
| smolvm 和 resourced 编译检查 | `cargo check --locked --offline -p smolvm -p smolvm-resourced --features smolvm/deep-runtime --bins` 通过。日志 `.build/kernel-cow-runtime/smolvm-bundle-check.log`。 |

复验入口为 libkrun 的 `kernel_cow_kvm` 示例。用同版工具链运行 `cargo build --locked --offline -p krun-vmm --features blk,net --example kernel_cow_kvm --target x86_64-unknown-linux-musl`。在专用 KVM 实验机上按顺序取得 `heavy.lock`、`worker-lab.lock`，将产物和上表对应的 bzImage 传给独立内核仓库的 `scripts/boot-cow-test kvm <bzImage> <kernel_cow_kvm> <serial-log>`。独立内核套件使用同仓库 `scripts/run-extended mm|kvm` 与对应 C 探针。二进制和内核摘要见 `.build/kernel-cow-runtime/{k1-mm-inputs,k1-inputs,libkrun-held-kvm-inputs}.sha256`。本地候选 gitlink 指向同机对象库中的 libkrun 提交。

## 初始候选的产品接入门槛

1. resourced 需要通过当前执行身份授权同一次 held cut，交付 pidfd、固定布局与 checkpoint，并在目标确认后收尾。当前 managed 路径在 smolvm 的 `src/runtime_ext/managed.rs::boot` 中先降权，随后才获取 Native RAM 和装配 libkrun。私有 libkrun ABI 已能在最终 VMM 进程降权前完成导入，仍需由该启动路径消费绑定到源执行与目标操作的一次性授权。现有 Native 请求继续使用原来的路径。
2. 用真实 managed 磁盘、卷和网络 Guest 验证相同捕获点的盘切层、活跃设备线程写入隔离、Guest 身份和网络入口。当前 VMM 实验带真实 VMM 设备与 Gate，Guest 是无磁盘与网络的最小程序。
3. 在 resourced 注册可重连的只读 anchor，绑定捕获 ID、执行身份和引用计数，再验同点批次/重试、anchor 丢失、祖先删除、生命周期。持久导出、Hibernate/Restore 和 Standby 要跑原计划 C07–C10。
4. 完成 C11–C12 的产品选择、正式 Worker capability、旧内核回退、Bundle 兼容和三组性能验证之后，再开放 `kernel_cow` 选择。实际合入产品候选还需要记录同一 Kernel/Runtime Bundle 的完整端到端验收。

内核补丁基线与现有 Worker 启动内核没有改变。新测试配置、Runtime 和本报告均在本地候选分支。测试只启动了隔离 Worker。

## Worker 安装和切换语义

需要切换的是运行 KVM 的 Worker。Worker 跑在 PVE 虚拟机里时，通过这台虚拟机的引导器加载补丁内核。Worker 内的业务 Guest 继续使用各自的内核。启用 CoW 前，已有 Guest 需要通过配套 Runtime 内存配置重新启动或恢复。

自托管阶段沿用 Boxd 的部署顺序。Boxd 对已有 KVM 主机通过 SSH 检查 sudo 和 `/dev/kvm`，验证发布包，由 Ansible 安装 Worker 内核、设置 GRUB 并重启，随后确认 Worker 注册。GKE 文档另提供 stock 内核选项，保留可用的 Fork 路径。[Boxd 安装流程](https://github.com/boxd-sh/self-host/blob/main/docs/INSTALL.md#install-on-bare-metal) · [Boxd 内核选项](https://github.com/boxd-sh/self-host/blob/main/docs/REFERENCE.md#host-kernel-and-secure-boot)

smolvm 发布时应配对固定内核与 Runtime Bundle，保留旧启动项，先在专用 PVE Worker 上重启验 KVM、CoW ABI 和产品 Fork，然后再接收工作负载。常规内核继续提供 Native。最初的 `worker` 配置用于临时 initramfs。新增的 `canary` 包已在一台隔离 Worker 上实测 GRUB 回退，生产签名和真实部署另行验收。

## 11 月 27 日可启动候选

Linux 补丁仓库 `5a2dfcd` 生成了 `smolvm-worker-kernel-canary_6.18.65-1+canary.0b0d03b4f5a2`，重复打包的 SHA-256 均为 `26f646cae966be123632b9ed4e862977cbc35c79ac8f54c195bbffa2b048eca1`。基于同一个 bzImage，MM 为 23/23，AMD 嵌套 KVM 为 6/6，通过两台子 Guest 在只读源退出后首次冷读和交错写验证。单次启动项实际引导磁盘上的 canary，运行跨 UID 探针，同一块盘第二次引导自动恢复 Ubuntu 6.8.0-151-generic 并成功运行 KVM Guest。两个内核的 taint 均为 0。PVE 宿主机、开发机当前启动项和承载业务的 Worker 均未变更。[可复核串口与输入摘要](https://github.com/Zhenyu-Sun-86587/deepfork-smolvm-archive/tree/5a2dfcd018b46d8bcb00c4955ab29e4581339bfd/evidence/canary-2026-11-27)

平台切片 `df3ef52` 支持在 System 模板创建机器时传 `memory_fork_profile=kernel_cow_anon_v1`。已启动的源通过当前执行的被动能力观察公布 `memory_fork`。Fork 的 `memory_fork_backend` 可选 `native`、`kernel_cow` 和 `auto`。不传仍请求 Native。固定匿名 RAM 源只允许其真实支持的提供者，对其他请求给出拒绝。旧 Worker 可继续走已经具备资格的 Native 路径，显式 kernel 请求在能力缺失时拒绝。选择发生在捕获之前。结果保留请求值、实际选择和可用的捕获配置；重复查询或重试绑定原 operation 与 capture。CLI 两个显式入口见下面示例，运行前先选择已登记的兼容模板和 Serve。

```sh
sandboxctl machines create cow-source --template "$SYSTEM_TEMPLATE" --boot-mode system --runtime-instance "$SERVE_ID" --memory-fork-profile kernel_cow_anon_v1 --start
sandboxctl machines live-fork cow-source cow-child --memory-fork-backend kernel_cow
```

现有的产品层分派仍分别提供独立全状态 Fork、任务分支和磁盘 Clone。内部 RAM 提供者只影响有资格的独立 Fork。`auto` 是用户主动选择的策略。平台拒绝把不同提供者的回执接到同一已受理捕获上。持久保存点和回退要分别检查兼容 reader、哈希和恢复出的真实工作负载。
