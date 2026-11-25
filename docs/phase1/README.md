# smolvm Sandbox Platform：第一阶段开发计划

> 2026-11-25 修订版：默认 Guest root；Live Fork 仅使用原生实时复制，不处理或提供应用安全点，活动工作负载不构成额外准入限制。Independent Clone/同逻辑 recovery 的既有捕获策略保持独立。

**版本：Phase 1 Plan v1.0 · 2026-11-25 · Asia/Shanghai**

本计划把上一轮研究收敛为一个可自主实施的工程目标：**在不修改 smolvm 及其底层依赖的前提下，完成可长期使用的 Linux 单节点 Agent Sandbox 产品。** 它包括管理后端、持久工作区、临时执行与持久任务、Live Fork 和 Independent Clone、快照/恢复/回滚、稳定访问入口、自动休眠唤醒，以及完整 WebUI、CLI、Python/TypeScript SDK 和基本运维。

本次交付保持原 5 份 Markdown、4 个里程碑与 11 个工作包。2026-11-25 用户修订：默认 Guest root（含 Exec、SSH、文件与 Persistent Task）；Live Fork 默认原生运行态复制，不以 Guest 用户身份、任务、会话、进程或 GTM 健康为准入限制。宿主仍为 root smolvm serve 与非 root platformd，原生隔离不变。原始规划到此定稿。后续实现的阶段结果、运行环境与验证范围见[本机验收](LOCAL_ACCEPTANCE.md)。

<a id="decisions"></a>
## 1. 已固定的决策

| 事项 | 第一阶段决定 |
|---|---|
| 上游边界 | 使用官方正式发行和配套依赖；不改 smolvm、内置 Guest Agent、libkrun、libkrunfw、Guest Kernel，不读写私有 DB，不调用私有运行时协议 |
| 架构 | 一个 Go `platformd` 模块化单体，集成 API、Gateway、生命周期、任务桥、快照目录、认证与事件；独立官方 smolvm serve |
| 运行环境 | Linux x86_64、Ubuntu 24.04、PVE 嵌套 KVM；8 vCPU/12 GiB 为初始目标环境，不作为容量承诺 |
| 网络 | 普通 virtio-net；Gateway 提供稳定域名/端口、认证、HTTP/TCP/SSH 访问和唤醒；不承诺稳定 Guest 私网 IP |
| 分支 | Live Fork 直接复用原生 Branch，并保留父子资源依赖；Independent Clone 用完整 checkpoint 注册全新 runtime，验证其不依赖源 VM 的生命周期 |
| 持久任务 | 自有轻量 Go Guest Task Manager＋runit＋独立 task runner；通过公开 Exec 启动与通信；普通命令仍用原生 Exec/Terminal |
| 存储 | VM 内部磁盘；平台管理原生完整 checkpoint 的目录、历史、导入导出和 pins；不开发新存储引擎 |
| 并发与恢复 | 平台持久操作、不可复用 runtime name、每实例协调、原 pause ID、结果未知时保守恢复；复用原生锁/恢复事务 |
| 资源 | 使用已有资源配置、监控、balloon、admission、pressure guard 和 cgroup/scope；平台只加简单重操作并发门和磁盘低水位检查 |
| 交付节奏 | 4 个内部里程碑、11 个实质工作包；全部属于第一阶段。Web/CLI/SDK 随真实接口交付，不拖到最后做空壳 |

### 实施基线

固定 **smolvm v1.37.1**，源码 commit **`9d3ab4c82756456565544558ac2ea5412afa84ef`**；正式发行时间为 **2026-11-25 04:03:13（上海）**。本次核对时 v1.37.2 只有 tag、没有对应正式 Release，因此不将其作为安装基线。[官方 v1.37.1](https://github.com/smol-machines/smolvm/releases/tag/v1.37.1)

此前实机证据仍属于 **v1.37.0 / `9b476839f529f3a4971f38f9999fb499a0309f8a`**。0→1 的关键变化涉及 Guest crun 临时执行文件与可选镜像 digest cache；一期只追加影响实施的版本契约和组合验证，不重新开展全领域调研或基础性能大评估。[版本差异](https://github.com/smol-machines/smolvm/compare/v1.37.0...v1.37.1)、[原实验复核](https://chatgpt.com/library?file_id=libfile_7eae0844ef548191b6b03681a7aea3fd&account_id=d0bce954-bca4-4a5e-a193-9001db1ee06b)

<a id="stack"></a>
## 2. 最终技术栈与常驻进程

| 层 | 已选方案 | 说明 |
|---|---|---|
| 平台服务 | Go、标准 net/http/slog、coder/websocket | API 与 Gateway 同进程；不增加独立 Gateway 服务 |
| 数据库 | SQLite、database/sql＋mattn/go-sqlite3、显式 SQL、WAL/FULL | 一个写者、短事务、版本化迁移；不引入 ORM/Redis/PostgreSQL |
| 运行时连接 | HTTP over Unix socket；有限公开 CLI 补口 | 私有 UDS；CLI 只接受固定子命令/argv/数据目录，进入相同操作协调 |
| 前端 | React＋TypeScript＋Vite＋xterm.js | 构建静态资源嵌入 platformd，无 Node 常驻服务 |
| CLI | Go/Cobra，命令名 `sandboxctl` | 所有业务操作走同一平台 API；本机 admin 命令负责受控维护 |
| SDK | Python/httpx；TypeScript/fetch/WebSocket | OpenAPI 基础类型＋少量手写流/等待/重连逻辑；不是官方 smol cloud SDK 的改名封装 |
| Guest | 自有 Go taskd/taskctl/task-runner、runit、OpenSSH | 内部持久 journal/输出，独立 runner 等待退出；不修改内置 Guest Agent |
| SSH | 客户端 OpenSSH＋认证 WebSocket ProxyCommand＋Guest sshd | 不自写 SSH 协议，不增加专用宿主 bastion 服务 |
| TLS 与 DNS | Go TLS＋已有 wildcard DNS；Caddy 可选 | Caddy 仅做证书/边缘反代；首版不要求 CoreDNS |
| 部署 | systemd、root serve、专用非 root platformd | 每次 serve 重建 UDS 后受控授权给平台组；VMM 原生降权和 confinement 保持启用 |

具体依赖 patch 版本在工程初始化时按兼容性固定到 lockfile 和发布 manifest；不能在实现中使用漂移的 latest 标签。首版常驻宿主进程只有 platformd、官方 smolvm serve 和实际运行的 VMM；可选 Caddy 不承担生命周期状态。

选择 root serve 是为了复用当前官方实现要求 root 的独立 systemd VM scope。**仍需真实验证 `systemctl restart` 的 VMM/任务接管行为**；不会以配置文件存在或单独 SIGKILL 试验代替它。[scope 权限前提](https://github.com/smol-machines/smolvm/blob/9d3ab4c82756456565544558ac2ea5412afa84ef/src/systemd_scope.rs#L421-L450)

<a id="scope"></a>
## 3. 产品范围与诚实的能力边界

### 第一阶段必须完成

| 产品面 | 可交付结果 |
|---|---|
| 实例 | 模板/镜像创建、配置、启动/停止/删除、完整 Pause/Resume、状态和错误解释 |
| 执行 | 普通命令、实时输出、Web Terminal、文件上传下载；独立 Persistent Task 的启动、状态、日志、取消与重连 |
| 复制与快照 | Live Fork、独立克隆、分支树/真实依赖、快照历史、导入导出、从快照创建、同逻辑实例恢复/回滚 |
| 网络与自动化 | 稳定 HTTP/HTTPS、经认证的 TCP/SSH 入口、服务域名/端口、并发 wake、保守 idle、显式 keep-running lease |
| 界面与集成 | 完整真实 WebUI、主要操作 CLI、可实际调用的 Python/TypeScript SDK |
| 运维 | 用户/Token/SSH key/客户端证书、私有 runtime、健康/日志/诊断、安装/升级/卸载、平台 DB 与快照备份恢复 |

### 必须随产品一起表达的限制

**Live Fork 保留完整运行态，只有一种原生实时复制语义。** 直接调用原生 Branch；允许 root、活跃 Task、SSH/Exec、服务、未登记后台进程及自定义镜像（须满足原生 profile）。不要求先停止工作、清空进程或冻结任务。第一阶段不设计或接入 Harness 安全点，不提供额外的安全分支模式、协作接口或等待流程。运行时可能短暂暂停以获得一致状态，父继续运行，子从捕获点继续。已有网络连接、外部请求及应用凭据不会自动修复或去重，UI/CLI/SDK 给出非阻塞风险说明。已复制的 Task 以新逻辑 ID 和来源关联接管原 runner，不重放 argv；证据不足标 unknown，仍保留子 VM。平台不暴露或编排上游 hold/waitReady、branch-ready/worker-ready 等分支安全点能力。Independent Clone 仍使用 clean standalone checkpoint 和独立闭包验证；其保守任务政策不适用于 Live Fork。[复制与恢复规范](SPECIFICATION.md#snapshots)

竞品依据：原《个人自托管 Agent Sandbox 研究》（2026-11-21）第 12–13、18、20、24–25 页区分 full-state snapshot/fork、特殊实时内存 Fork 与 Harness 安全点。多款竞品支持复制运行中进程，但各自存在配置限制。旧研究 ZIP `docs/01-research/competitor-analysis.md` §1.1 记录 [boxd Fork](https://docs.boxd.sh/guides/fork) 的官方行为：运行源短暂停顿后继续，子继承 PID/进程启动时间/uptime；平台 automation 默认暂停，不代表普通 Guest 进程必须停止。这里参考历史调研的产品语义，不声称已审计 boxd 闭源实现或重新实测所有竞品。

**Guest root 是正常产品能力。** 默认模板 UID/GID 0，SSH 使用授权公钥登录 root，普通文件路径覆盖 Guest 文件系统，允许安装软件、修改系统配置及运行服务。不将使用 root 标记为永久失效，不提供宿主 root 权限，不扩大 Guest 原生 capability/device/网络配置。GTM 状态用于任务管理和自动 idle 的可观测性，不决定 Live Fork 是否允许。

**同逻辑实例的 recovery/rollback 保留原定政策。** 已验证冻结的受管任务可作为 recovery checkpoint；旧 runtime 必须先实际停止（paused 对象先导出保护再 Delete 并确认不存在）。默认取消捕获任务，显式续接才解冻已有进程，绝不重新提交原 argv。该策略与 Live Fork 的原生进程继承分离；冻结验证失败只影响对应 recovery 能力，不影响 Live Fork 或已验证的 clean Clone。

**独立性必须验证。** 换一个 runtime 名称不能证明独立；需验证源实例退出/删除、隔离原缓存与路径后，新副本的 RAM、磁盘与后续恢复仍可用。独立于源 VM 不等于可以删除副本仍需的工件；有引用时继续 pin。[独立性与 pins](SPECIFICATION.md#snapshots)

**TCP 与 SSH 先认证再唤醒。** TCP 可由认证 CLI 隧道使用；直接公开的稳定 TCP listener 要求 mTLS。普通 SSH 客户端用生成的 ProxyCommand，Guest host key 正常校验。稳定域名是 Gateway 地址，不是 Guest 私网互联，也不保证旧连接跨完整休眠保持。[Gateway](SPECIFICATION.md#gateway)

**任务保护与紧急压力分开。** 活跃 Task、SSH/WS/TCP、Exec、显式 lease 会阻止平台正常 idle；原生 pressure guard 和宿主 OOM 仍可能暂停或结束实例。平台显示原因和真实任务结果，不自动重跑未知任务。[生命周期](SPECIFICATION.md#lifecycle)

### 简短的第二阶段清单

| 延后事项 | 当前替代 | 未来重新考虑的边界 |
|---|---|---|
| Named Network、稳定 Guest 私网 IP、直接互联 | 普通 virtio-net＋稳定 Gateway | 需要重新验证网络/完整状态兼容，可能涉及上游修改 |
| smolvm/Agent/libkrun/kernel 补丁 | 公开 API/CLI、自有 Guest 程序、明确拒绝不支持组合 | 独立评估上游能力和维护成本，不把补丁偷偷放进首版 |
| 原地/零复制分支脱钩 | 新 Independent Clone，保留原生父依赖 | 资源所有权和真实独立化代价 |
| 独立外部卷、复杂共享存储 | VM 内部工作区＋完整 checkpoint | 外部数据与 RAM 一致性、共享写与引用回收 |
| 高级调度、CoW 估算、资源账本 | 原生资源机制＋重操作计数门＋低空间保护 | 实际规模证明有需求后再设计 |
| 多节点、HA、跨平台迁移 | 单节点持久操作与明确故障恢复 | 节点归属/fencing、兼容和跨节点存储 |
| 企业多租户、深度 E2B 兼容 | owner/Token/简单 scope、自有 Agent SDK | 有明确集成需求时再增加，首版不预建空架构 |

<a id="reading"></a>
## 4. 五份文档怎样使用

| 文件 | 内容 | 主要使用者 |
|---|---|---|
| [README.md](README.md) | 阶段目标、固定决策、范围、阅读与推进方式 | 负责人、首次接手的 Coding Agent |
| [DESIGN.md](DESIGN.md) | 架构、仓库结构、各模块的复用/新增/数据/流程/失败/验收，Guest/Gateway/复制细节 | 设计与模块实现 |
| [SPECIFICATION.md](SPECIFICATION.md) | 原生接口映射、平台 API、数据库、状态机、并发恢复、任务协议、配置和错误 | 后端、前端、CLI/SDK 的共同契约 |
| [IMPLEMENTATION_PLAN.md](IMPLEMENTATION_PLAN.md) | 旧 W00–W17 处置、4 个里程碑、11 个工作包、依赖和自主推进规则 | 实际开发主入口 |
| [ACCEPTANCE_AND_OPERATIONS.md](ACCEPTANCE_AND_OPERATIONS.md) | 关键 gate、测试场景、安装部署、日常运维、备份恢复、升级卸载和完成标准 | 集成、发布、运维 |

首次接手按上表顺序浏览；开始编写代码后以 IMPLEMENTATION_PLAN 为主线，在每个包内查阅 DESIGN 和 SPECIFICATION 的对应章节。原研究包只在需要追溯依据时打开，不再把其全部工作包当实施要求。[原研究总览](https://chatgpt.com/library?file_id=libfile_e1e40dbcb0ac81918acf36710fc8fc67&account_id=d0bce954-bca4-4a5e-a193-9001db1ee06b)

<a id="workflow"></a>
## 5. 整体开发流程

| 第一阶段内部里程碑 | 实际结果 | 工作包 |
|---|---|---|
| M1 可管理的持久工作区 | 官方 runtime、认证/数据库/操作、基础生命周期、真实列表/详情/创建页面 | P1-W01–W03；W04/W10 起步 |
| M2 持久任务与完整状态 | 临时执行/文件/终端、Task、快照、Live Fork、Independent Clone、回滚 | P1-W04–W07；W10 持续 |
| M3 稳定访问与自动休眠 | Gateway、SSH/TCP/HTTP、活动与租约、自动 idle/wake | P1-W08–W09；W10 持续 |
| M4 产品与发布收口 | 完整 Web/CLI/两语言 SDK、安装升级、备份恢复、组合验收 | P1-W10–W11 |

关键开发关系：先完成统一生命周期的 EnsureRunning；任务和 Gateway 可以分别实现并上报活动；自动 idle 最后消费这些真实活动。这样不会出现“Task 等 Gateway、Gateway 等 idle、idle 又等 Task”的实施循环。UI、CLI、SDK 跟随每个真实模块推进。

Coding Agent 每完成一个工作包：完成有价值的模块/组合验证 → 更新实际开发记录与 API → 集成 → 自动继续依赖已满足的下一包。普通可逆工程问题自主解决，无须逐包等待用户确认。若某项能力确实需要底层补丁，记录固定版本的失败证据、禁用该具体能力、继续其他工作；不能以临时 patch 或模拟数据宣称第一阶段完成。

约 70% 精力用于功能实现、30% 用于必要验证。高风险重点是响应丢失/未知结果、并发 wake、父子依赖、独立恢复、Task 跨服务重启、冻结任务的恢复政策、路由身份和真实 systemd 重启。旧实验已经充分覆盖的基础能力不重复大规模验证。详细通过/失败判据见[验收文档](ACCEPTANCE_AND_OPERATIONS.md#acceptance-matrix)。

<a id="package-status"></a>
## 6. 本计划的交付状态

已完成旧计划的逐包重新分类、影响实施的公开 API 与正式发行核查、五份文档的接口/状态/目录一致性审查，以及 Markdown 链接、结构化示例和打包检查。源码证据直接放在对应章节；没有另附参考账本或 ADR 文件。

文档中的部署命令、测试与安装流程均是后续实施要求，**不是本轮执行结果**。尤其是 v1.37.1 实机兼容、Guest Task Manager、freeze/恢复、独立克隆与 Gateway 组合，需要 Coding Agent 完成代码后按计划验收。
