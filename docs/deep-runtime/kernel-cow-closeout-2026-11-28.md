# Worker Kernel CoW 合入交接

2026-11-28，亚洲/上海。明天从平台 `polish/integration` 和 smolvm `polish/runtime-candidate` 继续。运行验收使用一台隔离的 L1 Worker 和 AMD 嵌套 KVM。补丁 Linux 加载在这个 L1，测试 Guest 使用各自的内核。

## 版本

| 层 | 固定输入 |
| --- | --- |
| 平台、CLI、Web | 构建源码 `61dea3243b9d4ce3fea0fe9bd51256586ecc84c9`，开发 CLI 的 `--version` 为 `phase1-dev`。模型流夹具 `c0312af3d842d6ba78d45a92ab4271ccb6c844f5` 仅更新测试。 |
| smolvm / Worker | `e80e2579a6c3da2167b067573bed0677e2cd8304`，smolvm 1.37.2，Worker SQLite schema 32。 |
| libkrun | `b59c9bc832c7aec308f8f8f5095e0421921cfde1`，共享库 SHA-256 `73c903e69d861d700c9e1c9d791b9f55afe0859e8210efa0fca18d0aeee25308`。 |
| 实验 Runtime Bundle | `sha256:de5366c12f9bc0a5cc3dcf6bab2bc818d0d9b226a9e98ee8673ddd1245966f91`。开发构建的清单和制品摘要见 `.build/worker-kernel-closeout/bundle-e80e2579a6c3-73c903e6/build-record.json`。 |
| Worker 内核 | 6.18.65 canary，与之前 AMD 嵌套 KVM 6/6、内存测试 23/23 的内核一致。deb SHA-256 为 `16df7e6d4dd4f9b3c74d01f42f906da779718cd2287e9c3716a9efccf6415996`。stock 对照用 Ubuntu 6.8.0-151-generic。[内核包与启动证据](https://github.com/Zhenyu-Sun-86587/deepfork-smolvm-archive/tree/a4f76364095f63712299dd65603130c83d17ec14/evidence/canary-2026-11-27-landlock) |

## 修复

- Worker 重启后按原 execution、capture 和 attempt 恢复。已确认丢失的 anchor 结束未启动子项并释放预算，已成功的兄弟子机继续运行。Serve 在申请许可前等待目标进入准确的 systemd scope，只回收确认属于该启动且已空的 unit。活的 claim 继续由原进程认领。
- 新磁盘对象使用文件系统持久标识和含 inode generation 的文件句柄。Worker 每次仍校验当前打开的 FD 与独占 OFD 锁。原盘的只读复核确认 stock 的设备号从 `254:32` 变为 `253:32`，三份 backing 的 inode 保持一致。新保存点的精确恢复复用受验证的磁盘身份。旧不可变层可由已提交的 manifest 和 SHA-256 重新证明，原 manifest 保留。发生设备号变化且旧可写后继盘缺乏持久证明时，`exact` 返回 `E_BACKING_IDENTITY_UNPROVEN`。允许使用原保存点的场景可显式选择 `clone_conflicts` 来创建新的写头。
- 目标进入 `failed_held` 后，Worker 负责关闭对应执行并核实退出、写者和 TAP，再结束子操作。已有激活提交的结果仍保留不确定效果。Guest Gate 的负 errno 写入对应 VMM 的 stderr。父机只保留已获授权的原 relay，保护仍有效的同一父机连接可以继续收完流。
- 两条旧 SQLite v30 迁移各自增加了不同数据。新迁移测试覆盖 v29、两种 v30 和预览 v31，再打开数据库时保留已存在的 anchor 或 relay hold。

## 验证

原始结果在 `.build/worker-kernel-closeout/closeout-final/`。平台及 Worker 构建和验收串口固定到上表的 Bundle。关键结果和串口摘要在同目录的 `qualification.json`。

| 场景 | 证据 |
| --- | --- |
| Kernel CoW 批次与生命周期 | `kvm-e80e2579-61dea32/canary-runtime-kernel_cow.serial`。同点恢复两子，祖先退出后孙机继续，broker 重启后只重试失败子项，丢失 anchor 拒绝改用新捕获。通过有只读盘的保存点、Hibernate/Wake、回滚及新 epoch。 |
| 跨启动恢复 | `kvm-e80e2579-61dea32/{canary-cow-prepare-reboot,stock-cow-restore-reboot}.serial`。在 canary 上生成保存点并退出原执行，从同一磁盘启动 stock Worker。原保存点摘要、Guest PID 和运行 nonce 保留，机器恢复在新执行代。独立磁盘检查见 `stock-disk-identity.serial`。 |
| Native 和 Kernel CoW 模型流 | `streams-e80e2579-c0312af/{stock-protected-stream-fork,canary-protected-stream-kernel-fork}.serial`。两种路径下父机都在原连接收到 1 个前段、1 个后段，两个子机各读取 1 个前段和 0 个后段。原 POST 在受控上游只出现 1 次，每台子机另用各自授权访问。首次夹具配置碰到 CPU 准入和受信安装路径限制，修订夹具后使用同一份产品二进制通过。 |
| 凭据代理回归 | `kvm-e80e2579-61dea32/stock-proxy-revocation.serial`。常规 Worker 的凭据代理撤销用例通过。 |
| 后端兼容 | `kvm-e80e2579-61dea32/{canary-cow-native,canary-cow-auto}.serial`。同一 canary 可以沿用 Native，显式 `auto` 在符合条件时选择 Kernel CoW。回执报告实际选择。 |
| A14 激活效果 | `a14-activation-e80e2579/stock-a14-authority_prepared.serial` 的公开终态为 `failed`，`effect=none`。`stock-a14-policy_armed.serial` 在持久决议为 `commit` 后返回 `failed` 和 `effect=unknown`。两轮均确认失败目标退出、写盘与 TAP 围栏收敛、父机仍可用、后续成功子机三笔 UDP 收据可对账。[早期候选六个切点](a14-virtio-tx-qualification.md) |
| A14 顺序 Fork | `kvm-e80e2579-61dea32/stock-a14-sequential.serial`。常规 Worker 上，同一父机连续两次 Fork。独立接收器共对账 17 个不同的 UDP 业务请求，父机和两子机来源地址及身份各不相同。 |
| 静态与并发回归 | `runtime-e80e2579.log` 的 Worker 39 项通过、3 项需特权的测试忽略，Serve scope 18 项通过。VMM 179 项通过、7 项忽略。设备 306 项通过，一项环境受限的 socket buffer 断言单独登记，开发机上限低于断言预设。A10 进程间读写互斥和共享只读代验证见 `block-concurrency-final.log`。 |
| 平台和客户端 | 全仓 Go 测试、CLI 构建、TypeScript 和 Python SDK 合同、Vite、Chromium 表单、CLI 启动命令复制以及 Fork 回执均通过。日志在 `kvm-e80e2579-61dea32/platform-go.log` 和 `client-verification-full.log`。浏览器使用实编译控制台与受控 API。 |

## 使用

先在 Worker 的运行时清单确认对应模板、Bundle 和 `boot.kernel_cow_ram`。创建选用匿名 RAM 的 System Guest，再显式要求 Kernel CoW。旧脚本省略后端选项时继续默认 Native。显式 `auto` 依当前执行的 profile 和能力选择，回执分别记录请求值和实际后端。

```sh
sandboxctl machines create cow-source --template "$SYSTEM_TEMPLATE" --boot-mode system --runtime-instance "$SERVE_ID" --memory-fork-profile kernel_cow_anon_v1 --start
sandboxctl machines live-fork cow-source cow-child --memory-fork-backend kernel_cow
sandboxctl operations get "$OPERATION_ID" --json
```

其他 CLI 命令、等待与退出码约定见[常用工作流和 boxd 语义对照](cli-workflows.md)。

## 续做

平台从 `worktrees/polish-integration` 开发。Worker/Serve 从 `worktrees/polish-runtime-candidate` 开发，libkrun 的合入源码在 `worktrees/kernel-cow-libkrun`。Git 源码提交和实验 Bundle 分开固定，生产构建及安装沿用发布流水线。

前后端合入提交和 Mac CLI、菜单栏源码已经在平台集成分支。旧 B3 文件验证补丁和 Native 文件补丁的等价变更也已核对。多节点保留独立的 `worktrees/multi-node`，分支为 `feat/multi-node-v1@1c039e3791f1c3f0a83fa4d3356cabb376f694d3`。同机两 Node 的验收已记录，下一步是双物理 KVM Worker。

本轮的模型流验证使用本地受控 TLS 上游。托管 Agent 会话按已完成轮次截断，属于另行实现的产品合同。双物理 Worker、压力与长时寿命、生产下载与 OAuth、签名 Mac App 仍按[生产验收交接](production-handoff-2026-11-27.md)推进。

工作树回收台账为 `.build/worker-kernel-closeout/closeout-final/worktrees-reclaimed.json`。保留命名分支和可恢复的 detached 提交引用，保留有未提交内容的原目录。
