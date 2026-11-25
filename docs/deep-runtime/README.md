# smolvm 深度二次开发工程资料包

**版本：2026-11-25 · 设计基线 v1 · 目标：Linux x86_64 / 标准 Worker 内核 / 单节点优先**

本资料包把用户提供的《深度二次开发技术研究报告（修订版）》和新的工程要求转为可直接实施的设计。只适用于第三条路线：**定制 smolvm / libkrun / libkrunfw，加既有 platform-smolvm 的增量开发**。官方 Runtime 路线与 Cocoon 路线保持独立。本次交付是工程规格与实施计划；新协议、服务、接口和能力除明确标为“已有”外，均尚未实现，不应读成通过实测的产品承诺。

## 1. 目标与不可退让的语义

最终产品是可长期运行、可编程、可分支的 Linux 工作空间：普通虚拟机、个人云电脑、Agent 执行、多 Agent 协作共用一个平台。保留已有 Daytona WebUI、Go platformd、SQLite、Guest Task Manager、sandboxctl、Python/TypeScript SDK 和统一 Gateway。

- 原生 Live Fork 继续使用 memfd、CoW、不可变 RAM Generation、分层映射和独立子 VMM；不以全量 RAM 落盘、冷启动、任务重放或应用安全点代替。
- Fork 后父子拥有独立生命周期；共享资源属于 Worker 资源目录，不属于祖先实例。独立生命周期不等于 volatile RAM 能抵御 Worker 断电。
- 四种主状态：`Running / Standby / Hibernated / Stopped`；过渡、失败与管理健康单独表达。Standby 是 VMM 原生设备收束与暂停，Hibernate 是持久化完整状态后停止。
- **B：Native IP 为新建实例默认；A：Fixed Internal IP 可选。** API 枚举为 `native / fixed_internal`，Fork 默认继承，平台设置只影响之后创建的实例。本阶段不做运行中 A/B 无损切换。
- 自动生命周期是正式能力：全局默认与实例覆盖统一管理Standby/Hibernate定时器、常驻、回收优先级、运行租约、活动检测和按需唤醒；等待LLM响应不是idle，自动休眠资格不限制用户主动LiveFork。
- OCI 与真正以 systemd 为 Guest PID 1 的 System Profile 共用捕获引擎。Guest 管理 Agent 停止不应阻断 VMM 级生命周期控制。
- 系统盘和真实块设备 Volume 进入同一个捕获闭包；完整恢复绑定精确磁盘代际。不得旧 RAM 配新卷，也不得遗漏已挂接数据盘。
- 运行时版本、设备布局、Guest Kernel、网络/卷协议和状态格式组成不可变 Runtime Bundle。不能用 semver 推断兼容，也不能恢复失败后偷偷冷启动。

上述约束源于用户最新工程要求及修订报告第 5、7、10、11、14、15 节。尚未实现的能力通过 Capability 明确拒绝，不伪装完成。

## 2. 已核对的工程基线

本次实际通过 Git 获取与检查仓库、提交对象、文件和 submodule gitlink；GitHub 网页读取未提供可用正文，结论不依赖搜索摘要。核对日期为 2026-11-25（北京时间）。固定值也写入 [baseline-lock.json](baseline-lock.json)。

| 对象 | 实际核对结果 | 本工程采用 |
| --- | --- | --- |
| platform-smolvm | main = `582a6f141fedd7381acd54ff2cb11ae7a6e0f899`，Phase 1.5 本机验收提交；未观察到远端另一开发分支 | 从此提交建立 `deep-runtime/main`；不修改 main 或并行路线 |
| 用户 smolvm Fork | main = `53a04b92f3e7d1a50cb913db60ee0c55b3eadf42` | 下游开发分支以更新的上游提交为祖先 |
| smol-machines/smolvm | main = `5bd7c2f8e8e2d29869226a91f766bf4147b5e693` | 精确固定此提交，作为工程初始源码基线 |
| libkrun | smolvm gitlink = `36b0846191bf8dbfc044c98cf43922c2f1f221e4` | 使用 smol-machines 定制分支；不得换成通用 libkrun |
| libkrunfw | smolvm gitlink = `168e68a068c43a0fd48b71d1f52ca0081d079803` | 固定该提交；Guest Linux `6.18.60` |
| libkrunfw 新 main | `2f12ddce482f0956d7e9d4695e0f8a1bc38a1c9a` | 与上述 gitlink 的 tree 同为 `b30cd9d5af81e0c8ac10cfcf3f79b05d57adc327`；无需为了 HEAD 改动依赖 |
| smolvm 最新可见 tag | `v1.37.4` → `889ec606a08ad08eeb6e808e2358342a647a7f57` | 作为发行版参照，不冒充本工程源码身份 |

选择新 main 的理由很具体：相对研究时的 `53a04b9`，新增 [#2448](https://github.com/smol-machines/smolvm/commit/829dd655587292e0338507b6119d93223771868f) 修复旧 packed layer 在 per-VM uid 下的可读性及缓存修复；[#2449](https://github.com/smol-machines/smolvm/commit/5bd7c2f8e8e2d29869226a91f766bf4147b5e693) 在“VM 已启动但 OCI workload 启动失败”时返回 `workload_error`。共 7 个文件变化，底层 gitlink 不变。工程采用这两项已合入修复；并不声称新 main 已通过本项目 KVM 验收。main 中 Cargo 仍写 `1.37.2`，因此发布必须使用独立的下游 bundle ID，不靠该字符串判断版本。

### 本次已经做了什么

本地已在独立 worktree 建立平台 `deep-runtime/main`，起点是 `582a6f1`；smolvm 本地也建立同名分支，起点是 `5bd7c2f`。五份文档位于平台 `docs/deep-runtime/`。没有修改生产源码、推送远端、创建远端依赖 Fork、部署服务、运行新的 VM 性能测试或改动 PVE。`libkrun` / `libkrunfw` 个人 Fork 的匿名访问未能确认，因此仓库创建/权限核实仍属于 WP-00，不能宣称它们已存在或不存在。

## 3. 文档导航与阅读顺序

| 文件 | 用途 | 首次执行必须阅读的部分 |
| --- | --- | --- |
| [ARCHITECTURE_AND_DESIGN.md](ARCHITECTURE_AND_DESIGN.md) | 模块边界、资源归属、网络/存储/Guest、兼容与维护决策 | 全文，重点是 capture 与 identity 的一致性边界 |
| [TECHNICAL_SPECIFICATION.md](TECHNICAL_SPECIFICATION.md) | 协议、结构、状态、事务、API/SDK/CLI/UI、错误语义 | 全文；实现时作为跨仓库契约 |
| [IMPLEMENTATION_PLAN.md](IMPLEMENTATION_PLAN.md) | 里程碑、依赖图、工作包、代码入口、DoD | 先执行 M0，再按依赖持续推进 |
| [ACCEPTANCE_AND_OPERATIONS.md](ACCEPTANCE_AND_OPERATIONS.md) | 验证门、指标、构建、发布、隔离、故障恢复、上游同步 | 先读部署隔离和禁止事项；按改动面执行验证 |
| [baseline-lock.json](baseline-lock.json) | 可机器读取的本次源码基线 | 仅是源码锁；不是已构建 bundle manifest |

包中只有以上五份主 Markdown，另含源码基线 JSON 和完整性校验文件。原研究报告不重复装入；工程中的源码事实指向固定 SHA，设计项明确标“新增/拟实现”。研究报告只作为机制解释背景，不覆盖本包修正后的事务细节。

## 4. 关键工程选择

1. **保留 platformd 与 smolvm Serve。** 新增明确的 Runtime Adapter interface；官方与定制 Adapter 共享产品 API。产品业务不进入底层 smolvm CLI。
2. **一个 Worker 只新增一个 `smolvm-resourced`。** 它持有易失 FD、磁盘/卷资源引用、全局 writer fencing 与宿主附件；同一 deep 路线的多个旧/新 Bundle Serve 共用。它不接管默认 RAM 缺页数据路径，不按每页做 IPC。
3. **捕获使用统一事务、操作使用不同终止策略。** Live Fork/Checkpoint 可在不可变捕获点发布后恢复父机；Hibernate 默认保持 vCPU 与设备暂停，直到 durable commit 和源进程停止确认。持久化失败可明确回到原状态，不能丢掉捕获之后的执行。
4. **身份恢复不是普通 Agent 执行一条改 IP 命令。** Native Fork 的 Guest 用户态 gate、基础设施身份 helper、宿主网络隔离和 host-attested 授权共同完成。可行性实验位于关键路径早期，失败不能静默切成 A 或冷启动。
5. **Storage v1 使用现有 qcow2/稀疏 raw 和 virtio-blk。** 将附盘纳入捕获/恢复和 Volume catalog；raw+reflink、vhost、共享恢复缓存和局部 rebase 是有测量触发条件的优化。
6. **自动生命周期只有一个Worker决策者。** custom平台与各Serve不再分别自动pause；policy/lease经Worker确认生效，活动与睡眠在同一actor排序。保留压力采样与准入，替换上游独立pause/kill/resume。
7. **升级按 Bundle 并行承载。** 新实例使用新 Bundle，历史状态由匹配 reader/旧 Bundle 恢复；跨 Bundle 的 Volume 和网络排他在 Worker 全局协调。旧状态不会因一次包升级自动变成新格式。

## 5. Codex 执行入口

可将以下任务原样交给实施 Codex：

> 读取 `docs/deep-runtime/` 全部五份 Markdown 和 baseline-lock.json。先核对真实仓库状态与 AGENTS.md，再执行 IMPLEMENTATION_PLAN 的 WP-00、WP-01、WP-02。仅在独立 deep-runtime 分支/worktree 工作。按工作包依赖完成实现、必要验证、文档和提交；每完成一个包继续可执行的下一个，不逐包请求批准。保留原生 memfd/CoW Fork、B 默认/A 可选、完整状态语义和 Phase 1.5 产品界面。不因能力缺失伪装冷恢复、任务重放、快照截断或漏盘成功。把未完成的能力标为 unsupported/unknown。发生真实语义变更、破坏性迁移或影响其他路线时，给出具体证据与可审查方案后再请求用户决策。

首次实施应重新检查远端，但**不自动将新 HEAD 替换本包固定基线**。如果出现影响正确性/安全的上游修复，走本包的候选 Bundle 同步流程。普通编译错误、局部冲突、实现细节、测试环境修复和可逆部署到隔离环境，都在已授权范围内继续完成。

## 6. 完成定义

工程完成以 `ACCEPTANCE_AND_OPERATIONS.md` 的能力矩阵和关键风险验收为准，不以文件数量、PR 数量或性能宣传值为准。新的完整状态路径至少要证明：删除祖先后冷页/祖先盘可读、递归分支可继续、子实例独立休眠和恢复、broker 重启不误回收、多盘代际一致、B/A 身份与互联可靠、Agent 停止后生命周期可控、旧 Bundle 状态可恢复、并发准入有效。UI/SDK/CLI 必须呈现这些真实能力与失败原因。

本包没有声称这些门已通过。本次验证的是源码基线、具体修改入口、工程依赖及文档内部一致性；生产能力需由后续实现和隔离 Worker 验收建立。
