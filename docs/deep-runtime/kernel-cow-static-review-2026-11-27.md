# Worker Kernel CoW 静态审查交接

2026-11-27，亚洲/上海。开发已暂停。所有运行结果来自一台开发机上的隔离 Worker。补丁内核运行在可丢弃的 L1，业务 Guest 使用 AMD 嵌套 KVM。生产 Worker 和 PVE 宿主维持原部署。

请优先审查以下四个问题。每条意见请给出涉及的版本与代码行、触发顺序，以及能区分不同原因的一条回归用例。

## 1. Worker 重启后，同一批次的失败子机无法重试

在 8 GiB L1 中，同一个捕获点成功恢复两个子机。一个子机进入 Standby，再 Fork 出孙机。祖先退出后，孙机保留进程与两块盘。随后在另一批次中终止一个待激活的子机，重启 Worker broker，再在原批次重试。第一条启动错误是 `E_RECOVERING`。约 660 ms 后才出现重复的 `StartTransientUnit … already loaded`。4 GiB 的早期试跑在资源准入阶段就被拒绝，8 GiB 实验才到达上述重试路径。

`28a3b062` 的恢复判定识别 Native 的已提交文件描述符转移。已经发布的内核 CoW 内存由独立进程持有，在 catalog 中是有固定源执行、会话和 boot ID 的 `kernel_mm_anchor`。原判定遗漏了这种表示。恢复在等待子操作结束，子操作申请启动许可时又需要 Worker 已就绪。[故障版本的恢复判定](https://github.com/Zhenyu-Sun-86587/smolvm/blob/28a3b0624cc2afd7293bc8e54cc774d676edf1d6/crates/smolvm-resourced/src/catalog.rs#L464)

修复候选 `86b592c5` 已构建。9 项定向测试通过，包含本次新增的恢复回归。该版本的 KVM 重试在获得实验锁之前取消。审查重点是可用、明确丢失、父机已释放退出三种状态的判定，以及子项能否始终沿用原 capture 和 attempt。明确丢失的记录仅允许恢复流程继续处理失败，不能作为子机导入成功的凭据。[修复差异](https://github.com/Zhenyu-Sun-86587/smolvm/compare/28a3b0624cc2afd7293bc8e54cc774d676edf1d6...86b592c5aaee70616db15533dee9717b4f907875) · [定向测试](https://github.com/Zhenyu-Sun-86587/smolvm/blob/86b592c5aaee70616db15533dee9717b4f907875/crates/smolvm-resourced/src/kernel_mm_anchor_tests.rs#L105)

## 2. 普通子机启动也会卡在同名 systemd scope

在补丁内核上显式选择 **Native**，公开操作 `op_59291af783dc84515f3858be4d963369` 已释放父机，父机为 `RUNNING`，内存提供者记录为 Native，anchor 表为空。子操作仍处于 `NEW`，Serve 重复报告同名 scope 已加载，最终超时。在带运行租约的主动 Fork 中也见过该症状，同一二进制再次运行则完成，父机等到了原请求的 HTTP 响应。该复现路径没有主动重启 broker。目前仍缺首次异常时 scope 中的 PID、启动时间和 Worker 的启动认领记录。

请检查 Serve 从创建子进程、加入 scope、签发许可、交付启动参数到注册执行的顺序，尤其是中途失败或重启后，谁持有原子进程、谁可以终止它、下一次调度会认领哪个进程。应保留现有执行身份与写入授权核验。简单删除同名 scope 可能误伤原有执行。[Serve 启动与确认](https://github.com/Zhenyu-Sun-86587/smolvm/blob/86b592c5aaee70616db15533dee9717b4f907875/src/runtime_ext/managed.rs#L139) · [scope 创建](https://github.com/Zhenyu-Sun-86587/smolvm/blob/86b592c5aaee70616db15533dee9717b4f907875/src/systemd_scope.rs#L583) · [Worker 认领](https://github.com/Zhenyu-Sun-86587/smolvm/blob/86b592c5aaee70616db15533dee9717b4f907875/crates/smolvm-resourced/src/managed.rs#L516)

## 3. 从补丁内核切回 stock 后，原保存点的磁盘身份校验失败

补丁内核完成 Hibernate 并确认源 VMM 退出。同一块持久测试盘在 stock 内核重新启动后，平台找回原保存点 `point-de1824f401c5bc27500cb7dcca96871d` 及相同的 manifest 摘要 `sha256:563beac7bbbbe677630bb1fa2080e1d65ef1e7256564ea109eed1526775d4863`。同机精确恢复 `op_481c5f7f3604f8ba7841e54b30fed489` 在打开根盘链时反复返回 `E_HOST_IO: managed block backing identity changed`。

根盘首文件记录为 `inode:65056:262218`，设备号是 `254:32`。原 ext4 盘上根盘、基础镜像和独立数据盘的 inode 仍分别为 `262218`、`262186` 和 `262203`。stock 启动时对应文件的实际 `st_dev` 尚未采样。设备号重分配是待验证的解释。`open_final` 的同一个错误还覆盖 mode、硬链接数及 raw 文件长度。请审查持久文件身份与本次启动的文件描述符身份分别应如何验证，并覆盖历史保存点、同名文件替换与原写者仍持有独占锁的情形。[磁盘文件验证](https://github.com/Zhenyu-Sun-86587/smolvm/blob/86b592c5aaee70616db15533dee9717b4f907875/crates/smolvm-resourced/src/blocks.rs#L114) · [保存点打开磁盘链](https://github.com/Zhenyu-Sun-86587/smolvm/blob/86b592c5aaee70616db15533dee9717b4f907875/crates/smolvm-resourced/src/statepoint_capture.rs#L431) · [跨内核测试](https://github.com/Zhenyu-Sun-86587/platform-smolvm/blob/5c4d3e7fd298c210125946d523a8a43da637a746/tests/integration/kernel_cow_fork.py#L108)

## 4. 父机原模型连接与偶发的子机激活失败

这条测试使用 stock Worker、真实 Guest 和本地受控的 TLS 流式服务。Guest 提交一次 POST，收到首事件后执行完整状态 Fork，服务等子机激活后才发末事件。此前一次失败中，Guest 探针因带超时的 `read` 返回 `EINTR` 提前退出。修正探针在同一文件描述符继续读后，单子机场景通过。等待 31.77 秒时原 reader 和运行保护均在。父机首末事件各 1 次，子机首事件 1 次、末事件 0 次，上游原 POST 共 1 次。两子批量及主动撤销在本候选上尚未运行。[Runtime 的原连接保护](https://github.com/Zhenyu-Sun-86587/smolvm/commit/87fac7a5fff7a8a7e68e5a581ef550497d568c2b) · [VMM 的源与目标连接处理](https://github.com/Zhenyu-Sun-86587/libkrun/commit/a8fc17ec3b837828856409db85c4b66177128c21) · [单子与批量验收条件](https://github.com/Zhenyu-Sun-86587/platform-smolvm/blob/57a0036377f6e2105e55b0d9679189f4af701ad7/tests/integration/guest_control_kvm.py#L272)

更早一次单子实验中，父机已释放，子机的激活状态为 `failed_held`，决议为 `none`、revision 为 4，公开操作仍为 `observing / unknown`。该次失败缺 Guest 内部 errno。请检查目标身份激活失败如何反馈给原操作，能否在证据不足时保持效果未知，并确保查询或重试不会重放上游 POST。托管 Agent 会话截到最近完整对话轮次、子机会话等待新输入，是另行实现的产品合同。[目标诊断候选](https://github.com/Zhenyu-Sun-86587/libkrun/commit/d2e9f65929154f717b91fd711785395813c8781c) · [会话产品合同](https://github.com/Zhenyu-Sun-86587/platform-smolvm/blob/5486d9a68e310fb9df2bc3b957c1a5ffaead6c84/docs/deep-runtime/production-handoff-2026-11-27.md#L114)

## 已通过的边界与尚缺的证据

| 范围 | 截止暂停时的证据 |
| --- | --- |
| 内核原语和引导 | 内存管理 23/23，AMD 嵌套 KVM 6/6。只读源退出后，两台子 Guest 首次读取均为原值，之后可各自写入。隔离 Worker 的补丁内核及 stock GRUB 启动、KVM 和 taint 均已核验。[内核包与串口](https://github.com/Zhenyu-Sun-86587/deepfork-smolvm-archive/tree/a4f76364095f63712299dd65603130c83d17ec14/evidence/canary-2026-11-27-landlock) |
| 公开内核 CoW Fork | 单子完整状态、父子 RAM/根盘/数据盘独立、双向私网访问和请求去重均通过。显式 kernel 与 auto 的真实选择均通过。auto 的外层运行表误记失败，Guest 串口记录了后端选择、成功退出和正常关机。未发布 anchor 丢失时，失败回执为 `none`，同一父机恢复运行。 |
| 生命周期和网络 | 同一补丁内核上的 VM 保存点、Hibernate/Wake 恢复了原进程内存标记和两块盘。另一个已固定 stock Bundle 的 A14 网络验收 7/7，两次 Fork 后父机和两台子机共 17 笔 UDP 业务回执匹配。[A14 报告](https://github.com/Zhenyu-Sun-86587/platform-smolvm/blob/5486d9a68e310fb9df2bc3b957c1a5ffaead6c84/docs/deep-runtime/a14-virtio-tx-qualification.md) |
| 时延与复制字节 | 首个正确业务响应单样本为 stock/Native 17.694 秒，补丁内核/Kernel CoW 17.902 秒。后者受理 7.45 毫秒，进程内存导入 10.945 毫秒。补丁内核/Native 卡在子机启动。三组对照尚缺一组，实际复制字节探针已提交，KVM 测量未开始。[计数夹具](https://github.com/Zhenyu-Sun-86587/platform-smolvm/commit/62b20d85a338fdec9aec7b75e77b4827351df133) |
| K4 客户端 | 单独分支 `feat/kernel-cow-client`。CLI、两套 SDK 的合同测试、Web 生产构建和 Go 嵌入编译通过。Chromium 首次停在选项禁用断言；已修测试断言，复核于启动前取消。[客户端源码](https://github.com/Zhenyu-Sun-86587/platform-smolvm/commit/227fa53094739d067a9876b55f25e0c45bb4bd27) · [嵌入构建](https://github.com/Zhenyu-Sun-86587/platform-smolvm/commit/09f2358c0d430b801473f49191b25790dd44394d) |

## 版本及原始记录

| 复现对象 | smolvm / libkrun | 实测 Runtime Bundle |
| --- | --- | --- |
| 主要 Kernel CoW 复现 | `28a3b0624cc2` / `2ad57a8ddfba` | `sha256:bddf4dfab7a46eb806c9adf4eb8869ee268d4c14f69ecacbe20b3f78590ca03c` |
| Worker 恢复修复，已构建和单测 | `86b592c5aaee` / `2ad57a8ddfba` | `sha256:b103bbff408d59d609384e10e59b845d9c256704e19473c79d957509fc5d149e` |
| stock 单子模型响应流 | `ec418d0155ee` / `d2e9f6592915`，平台夹具 `57a0036377f6` | `sha256:1646d05d412794533462257fa657b678a32f0b6f3852773d88eccdab5ac9a945` |

本机证据根目录是 `/home/dev/workspace/deepfork-smolvm/.build/worker-kernel-closeout/`。下表的行号按原始串口 LF 计数。开头为 `followup-` 的目录对应第一行 Bundle。

| 原始记录，相对于证据根目录 | 内容 |
| --- | --- |
| `followup-28a3b0624cc2/runtime-diagnostics/canary-runtime-kernel_cow-8g-diagnostics.serial:458` | broker 重启后的首错与紧随其后的 scope 碰撞 |
| `followup-28a3b0624cc2/canary-cow-native.serial:442` | Native 选择、源已释放、无 anchor、子机未完成 |
| `followup-28a3b0624cc2/stock-cow-restore-reboot.serial:630`、`followup-28a3b0624cc2/stock-storage-identity/catalog-identity.json` | 跨内核恢复错误、同一保存点和三份文件身份 |
| `stream-stable-01bef1f8bccb-diag-3a37c8c-protected-stream-single.serial:615` | 早期子机激活停在 failed_held |
| `stream-stable-ec418d0155ee-protected-stream-single.serial:617`、`stream-stable-ec418d0155ee-eintr-57a0036-protected-stream-single.serial:614` | 同包下探针遇 EINTR 退出、修正后在原连接读到唯一末事件 |
| `build-86b592c5.log:94`、`build-86b592c5.log:103` | 新恢复回归及 9 项总结果；同一日志后面的独立测试筛选匹配了 0 项 |
