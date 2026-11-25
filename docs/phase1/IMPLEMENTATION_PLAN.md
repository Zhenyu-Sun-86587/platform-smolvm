# 第一阶段实施计划

> 2026-11-25 修订版：默认 Guest root；Live Fork 仅使用原生实时复制，不处理或提供应用安全点，活动工作负载不构成额外准入限制。Independent Clone/同逻辑 recovery 的既有捕获策略保持独立。

<a id="plan-scope"></a>
## 1. 目标、基线与执行原则

**第一阶段完成一个能长期使用的单节点产品。** 交付包括统一生命周期、内部持久工作区、Live Fork/Independent Clone、快照历史/导出导入/恢复回滚、独立持久任务、稳定 HTTP/TCP/SSH 入口与自动 idle/wake、完整 Web/CLI/Python/TypeScript，以及安装升级、备份恢复和排障。4 个里程碑全部在第一阶段内部，不以最小 CRUD 作为完成点。

本轮交付是设计与可自主执行的开发计划，**尚未实现平台、部署服务或执行下列新验收**。上一轮 PVE 实验使用 smolvm v1.37.0；拟实施基线为已正式发布的 **v1.37.1**，commit `9d3ab4c82756456565544558ac2ea5412afa84ef`，发布于 `2026-11-24T20:03:13Z`。v1.37.2 当时只有 tag，不作为可部署官方 Release。v1.37.1 不借用旧实测结果宣称复测通过，先做变更触及路径与本计划窄门验证。[v1.37.1 官方发行](https://github.com/smol-machines/smolvm/releases/tag/v1.37.1)、[0→1 差异](https://github.com/smol-machines/smolvm/compare/v1.37.0...v1.37.1)

运行目标为 Linux x86_64、Ubuntu 24.04、PVE nested KVM 的首个 8 vCPU/12 GiB 节点。采用 Go 模块化 platformd 单体、SQLite WAL/FULL、独立官方 smolvm serve、自有 Guest taskd/taskctl/task-runner 与 runit；Web 静态资源嵌入同一进程。runtime 通过私有 HTTP UDS 接入，公开 CLI 只填有记录的接口空缺；**不修改 runtime、原生 Agent、libkrun/libkrunfw、kernel，不访问私有 DB 或私有控制协议，不建私有 patch queue。** 具体规范以 [SPECIFICATION.md](SPECIFICATION.md) 为准，模块设计见 [DESIGN.md](DESIGN.md)，验证与操作步骤见 [ACCEPTANCE_AND_OPERATIONS.md](ACCEPTANCE_AND_OPERATIONS.md)。

### 自主实施约定

- Coding Agent 继续普通工程选择、目录划分、测试 fixture、错误修复与集成，不为可逆实现细节反复询问。每包完成以真实 API 和验收证据为准，不以 mock、截图或编译通过替代运行行为。
- 若某个公开能力不足，先给出固定版本请求/响应与最小复现，隔离该 profile/功能，继续无关工作；不修改底座、不私自降低必需功能、不假装完成。独立任务基本持久能力若无法实现，必须报告为实质 blocker。
- 任务与源码版本先固定。Go/Node/Python 包的具体版本由实现 lockfile 与 release manifest 固定，不在计划中凭记忆指定“最新 patch”；官方发行资产按清单和 checksum 验证来源/完整性。
- 功能实现约 70%、针对风险的验证约 30%。验证放在模块完成边界，复用旧 PVE 证据；不重开完整生态调研、VMM/内核审计或全网络矩阵。
- 交付代码只能操作平台拥有的数据。删除 runtime、制品、备份或恢复旧 DB 走明确管理操作，危险操作结果 unknown 时不盲目补偿或重试。

<a id="old-work-packages"></a>
## 2. 上一版 W00–W17 的重新评估


下表逐项重新评估上一版 W00–W17。“保留、重写、简化、后移、删除”是实施处置：后移项不构成第一阶段前置；删除项不应以改名方式重新进入代码。

| 旧包 | 处置 | 第一阶段保留的具体结果 | 移走或删掉的内容 | 新依赖/落点 |
|---|---|---|---|---|
| W00 底层组合与故障门槛 | 简化 | 复用原 PVE 证据，固定官方候选 release，补公开接口与产品集成所必需的窄测试 | Named/full-state、host mount 组合、全内核/后端矩阵、重新建立基础研究课题 | P1-W01；各包的定向验收，不阻塞全平台 |
| W01 Runtime Adapter | 重写 | HTTP UDS、受限 CLI、公开 runtime 身份观测、原 pause ID、固定版本能力和错误映射 | B1 只读 API 补丁；嵌入 SDK；私有 DB/protocol；补造 execution ID | P1-W01；不依赖 W03，不等底层全矩阵 |
| W02 Registry/journal/auth | 保留并简化 | 逻辑机器 ID、不可重用 runtime 名、操作/幂等/事件、单写者、基本用户与 token/对象授权 | 资源 reservation 受理事务、企业 tenant/RBAC/OIDC 体系、复杂权限继承 | P1-W02，接 P1-W01 的接口草案即可并行 |
| W03 资源与启动协调 | 拆分，大部分后移 | 启动 reconcile、未知操作观察、已有 CPU/内存/磁盘配置与指标、磁盘临界拒绝、重型操作并发限制 | 原子字节预算、样本水位/unaccounted、CoW 预测、租户精确配额、平台 OOM 受害者选择 | P1-W03/P1-W11；不作为 Web/CLI/任务的高级资源前置 |
| W04 API/CLI 闭环 | 保留并扩展 | 统一平台 API、CLI、OpenAPI、操作查询、原生 exec/files；从第一里程碑给真实 UI/API | “只完成最小 CRUD 即第一阶段完成”的边界 | P1-W02/P1-W03/P1-W04/P1-W10；与后续功能迭代 |
| W05 holds/idle | 重写 | 显式 keep-awake、任务/exec/WS/SSH/连接 hold、suspect 阻 idle、pause 最后检查、wake 合并 | B6 bridge；把 runtime pool expiry 当任务租约；宣称 hold 抗 emergency/OOM | P1-W09；EnsureRunning 核心提前放 P1-W03，避免 Gateway 与 idle 互相依赖 |
| W06 HTTP/TCP wake | 保留并调整认证 | HTTP 鉴权、稳定服务、背压、route 版本、singleflight、forwarding 三态；TCP 鉴权隧道和显式 mTLS listener | VIP/TUN、按 DNS 名推断 raw TCP 目标、未认证 raw TCP 默认开放；高级 reservation | P1-W08；依赖 P1-W03 EnsureRunning，不等 P1-W09 自动 idle |
| W07 SSH/DNS | 重写部署方式 | Guest OpenSSH + `sandboxctl` 的 HTTPS WebSocket `ProxyCommand`，已有 wildcard DNS 指向 Gateway | 独立宿主 sshd bastion/forced-command 服务、CoreDNS 常驻硬依赖、ProxyJump 扩展、Guest 私网 IPAM | P1-W04 模板 + P1-W08 隧道；正常 SSH/SFTP 单独验收 |
| W08 分支/独立化 | 重写 | Live Fork 依赖展示与父生命周期拒绝；Independent Clone 新 runtime；逻辑树与物理依赖区分 | 原地 detach、零复制脱钩、平台 unlink golden/layers、整棵树自动拆解 | P1-W07；Live Fork 依赖 P1-W03，P1-W05 完善任务接管；Independent Clone 再依赖 P1-W06 |
| W09 snapshot/backup/GC | 保留并简化 | 快照目录/历史、导出导入、增量依赖闭包、独立恢复点、基本备份、操作保护引用、只清理平台自有制品 | 自研 CAS/存储引擎、跨节点对象调度、高级磁盘 reservation；外部卷一致性 | P1-W06；本地不可变制品目录和 runtime 格式复用 |
| W10 Guest 持久 Task | 全部重写实现路径 | 独立 Guest Task Manager，持久 task ID/journal/log/exit/cancel/reconnect/daemon 恢复/继承裁决 | B2 对原 Agent/protocol/serve 的修改；把 native exec ID 包装成持久任务；以 tmux 替代产品任务 | P1-W05；P1-W04 公开 exec/files 引导安装，不依赖 Gateway 或上游补丁 |
| W11 配额与外部卷 | 后移 | 只把内部磁盘容量配置、日志上限、Host 低空间基本保护吸收至 P1-W03/P1-W11 | Named Volume、外部卷版本/attachments、共享存储、B3 删除补丁、物理 CoW 归属配额 | 第二阶段候选；P1-W06 无此外部卷前置 |
| W12 Named 网络/IP | 后移 | 无首期实现；保留明确不支持声明 | Named+full-state、B5、稳定 Guest IP、私网互联、DNS 重绑定协议、TAP/netns 重构 | 不再位于第一阶段关键路径 |
| W13 发布/安全/升级 | 保留并重写 | 官方发行包与配套资产 pin、校验、安装升级、平台 DB 迁移、旧备份兼容、回退条件 | 私有 runtime patch queue、重建 VMM/kernel 的首期流水线、所有平台发行矩阵 | P1-W01/P1-W11；不暗示官方 archive 已签名 |
| W14 Web/SDK/templates | 保留，提前并行 | 完整 WebUI/CLI/Python/TypeScript；真实状态/任务/服务/树/历史/资源/错误；模板版本 | 旧文档“小页面即可”、可选 WebTerminal、把全部 UI 放最后；E2B 兼容层 | P1-W04/P1-W10 跨 M1–M4；完成标准不缩减 |
| W15 故障/容量验收 | 保留并聚焦 | 平台重启/serve 重启、wake 并发、请求不重复、任务归属、源资源移除、备份/rollback、安装升级 | 完整新容量研究、全后端/Named/外部卷矩阵、重新审计内核与逃逸、所有 fault 点遍历 | 各包自带有价值集成验证，P1-W11 汇总一次代表性长跑 |
| W16 多 Worker | 后移 | 单节点代码保留一个固定 node 标识即可 | 多主/fencing/节点调度/跨节点迁移/远端存储协调 | 第二阶段，不设计首期分布式协议 |
| W17 macOS/HVF | 后移 | 无首期实现；不破坏基本 Adapter 边界 | Mac/HVF release/test matrix、跨架构恢复 | 第二阶段，不扩充首期 CI 成本 |


### 关键依赖已改写

| 旧关键包 | 新契约与依赖 | 不再需要的前置 |
|---|---|---|
| W01 | P1-W01 使用已公开的 runtime identity、parentMachine、stateReason、ports；公开证据缺失时按 unknown 保守处理 | B1 接口补丁、全底层矩阵、embedded SDK |
| W03 | P1-W03 保留 startup reconcile、count semaphore、磁盘阈值与现有指标；未决操作重建一个重型执行位置 | RAM/磁盘字节 reservation、sample watermark/unaccounted、CoW 预测 |
| W05 | EnsureRunning 核心在 P1-W03；P1-W05 任务、P1-W08 连接都能产生 hold 后 P1-W09 才开启自动 idle | B6 runtime hold bridge；不把 OOM/pressure 作为正常 idle 可控制行为 |
| W08 | P1-W07 Live Fork 先依赖 P1-W03 原生生命周期；P1-W05 增加子任务接管；Independent Clone 另依赖 P1-W06 clean standalone 制品 | 原地 detach/自管共享层；Live Fork 不等待 GTM 健康或独立存储引擎 |
| W09 | P1-W06 本地不可变制品 catalog、引用保护、standalone 闭包与恢复验证 | 外部卷/B3、CAS/分布式对象库、精算磁盘预算、增量格式必需 |
| W10 | P1-W05 自有 Go Guest 程序，公开 background exec 引导 runit，taskctl 短 exec→Guest UDS | B2 原 Agent/protocol 扩展、公开 Task TCP 端口、Gateway 前置；不能以 tmux 或普通 exec 冒充 |

旧“W05→W06→W10→W05”的实施循环被拆开：串行生命周期先提供 EnsureRunning，任务和 Gateway 分别上报活动，最后组成 idle。默认保留 runtime 的压力保护；任务 hold 只阻止平台正常 idle，不声称拦截 native emergency 或内核 OOM。

<a id="milestones"></a>
## 3. 四个内部里程碑


11 个工作包全部属于第一阶段。P1-W10 跨里程碑并行；M4 是完整收口，UI/SDK 从 M1 开始交付。

| 里程碑 | 形成的可用结果 | 主工作包 | 阶段内 UI/CLI/SDK 增量 | 放行条件 |
|---|---|---|---|---|
| M1 可管理的持久工作区 | 官方 runtime 接入、身份/鉴权/操作、基础生命周期与重启协调 | P1-W01–P1-W03，P1-W04 最小模板/exec；P1-W10 起步 | 真实登录、列表/详情、创建与生命周期、操作进度、CLI 与 SDK 基础链 | Create 响应丢失不重建，platformd 重启不杀 VM；unknown 可见且有处置入口 |
| M2 可继续工作的任务与状态 | 内部工作区、临时终端/文件、独立持久任务、快照历史、Live Fork/Independent Clone/rollback | P1-W04–P1-W07；P1-W10 持续 | 任务/日志/取消、终端、分支树、历史、导入导出；SDK task/snapshot/clone | Task 不依赖 session，Live Fork 保留父子各自继续执行且不重放 argv，独立副本在源资源移除后仍可恢复 |
| M3 稳定访问与自动 idle | HTTPS/WS/TCP/SSH 稳定入口、预鉴权 wake、任务/连接 holds、自动休眠 | P1-W08–P1-W09；P1-W10 持续 | 服务端口与域名、SSH 配置/隧道、idle 配置/阻塞说明、资源与压力 | 未授权不 wake；同机并发 wake 一次；POST 不重复；任务等待时不正常 idle |
| M4 完整产品与可维护发布 | 全 UI/CLI/Python/TS 功能完整、安装升级/卸载/备份恢复/排障、自主实施验收汇总 | P1-W10 收口、P1-W11；各包修具体缺口 | 所有页面和命令接真实 API，无假状态；诊断与设置完整 | 从干净安装到任务/分支/访问/备份/重启/升级的真实用户流程全部通过 |

### 并行与关键路径

- P1-W01 Adapter 契约与 P1-W02 schema/auth 可并行；P1-W03 不等待高级资源账本。P1-W10 在 P1-W02 API 初版后开始，客户端使用真实 backend，不以 mock 充当交付。
- P1-W04 模板/exec/files 早于 P1-W05 Task Manager 引导；P1-W05 与 P1-W06 snapshot catalog 可以并行，双方共享中央 capture/继承状态契约。
- P1-W07 原生 Live Fork 可先接 P1-W03，P1-W05 完善任务接管；Independent Clone、restore/rollback 必须等 P1-W06 可独立读取的制品与 P1-W05 继承裁决。不能为了分支图绕过这个门槛。
- P1-W08 Gateway 只需要 P1-W03 的显式 EnsureRunning、P1-W04 SSH 模板与 P1-W02 授权，可与 P1-W05–P1-W07 并行。P1-W09 在真实 task/connection hold 都可用后开启自动 idle。
- P1-W11 的安装、日志和诊断从 M1 做起，M4 才做组合验收和发布包。用户工作约 70% 是功能实现，30% 是针对上述真实风险的验证；比例不要求记录工时精算，不安排独立“大研究阶段”。


<a id="repository-layout"></a>
## 4. 代码布局与共用契约

```text
cmd/                    platformd、sandboxctl、sandbox-taskd、sandbox-taskctl、sandbox-task-runner
internal/runtime/       固定版本公开接口适配，HTTP UDS 与受限 CLI
internal/registry/         SQLite、迁移、短事务与只读查询
internal/operations/    幂等受理、操作/锁、进度、事件
internal/lifecycle/     统一动作计划、EnsureRunning、恢复协调
internal/tasks/         Host 任务目录与 taskctl bridge
internal/gateway/       HTTP、WS、TCP/SSH 隧道、授权后 wake
internal/snapshots/     本地 catalog、导入/导出、保留与恢复
internal/auth/          用户/token、对象授权、短 stream ticket
web/                    React/TypeScript/Vite/xterm.js；构建后嵌入 platformd
sdk/python/             平台 API、httpx、任务日志与操作等待
sdk/typescript/         平台 API、fetch/WebSocket、任务日志与操作等待
guest/templates/managed/      OCI 模板、runit、自有 Guest 程序与 sshd
packaging/              官方 runtime manifest、systemd、安装维护脚本
tests/                  单元契约、窄集成场景、产品流程
```

这是目录清单，不是增加常驻服务。SQLite driver 固定 `database/sql` + `mattn/go-sqlite3`，显式 SQL/前向迁移、WAL/FULL/foreign_keys、单写者；标准 `slog` 日志；CLI 使用 Cobra，Go WebSocket 使用 coder/websocket。无 ORM、Redis、消息队列或分布式 scheduler。

平台 JSON 为 snake_case，base `/v1`；runtime JSON/路径转换全部留在 Adapter。所有调用者使用同一 `/v1/operations/{id}`、任务 output cursor、鉴权与 `If-Control-Epoch`/`Idempotency-Key`。共享字段及状态只在规范文件定义，工作包不得另加一组互相竞争的“正在恢复/已恢复/ready”枚举。路由只做进程内 cache，key 为 binding_generation、runtime identity 和 service_revision；重启或观测变化全部失效，不另建持久 routes/reservation 表。

<a id="work-packages"></a>
## 5. 11 个工作包


以下为可直接交给 Coding Agent 的平台自有工作包。字段中的接口/状态以 [SPECIFICATION.md](SPECIFICATION.md) 为唯一规范；禁止改 smolvm、原生 Guest Agent、libkrun/libkrunfw、kernel 或其私有数据/协议。目录属于拟建代码仓库，本轮没有创建这些实现。Guest cmd 目录使用 sandbox- 前缀，模板安装名为 taskd/taskctl/task-runner。

### P1-W01 官方发行物、Runtime Adapter 与支持配置

| 字段 | 内容 |
|---|---|
| 目标 | 将固定官方 smolvm 发行物作为独立服务接入；准确映射公开能力、结果和不可判定状态 |
| 前置 | 用户 PVE 测试证据、官方候选 release 定向差异；无 W00 全矩阵前置 |
| 目录 | `internal/runtime/smolvm/`、`internal/runtime/types.go`、`internal/config/`、`packaging/runtime-manifest.json` |
| 上游复用 | serve HTTP over UDS、公开 machine/ports/pause/resume/branches/checkpoint/exec/files/capacity；仅公开 CLI 补齐未覆盖入口 |
| 具体任务 | 固定 binary/依赖 digests；版本探测；建立普通 virtio-net+内部存储支持 profile；结构化 CLI argv 与受控工作目录；需要以 runtime 身份运行的公开 CLI 通过 root-owned allowlist wrapper/oneshot，仅接固定子命令与已核验平台参数，不授任意 sudo shell；machine_id/binding_generation/runtime_name 分开，原 runtime_pause_id 与平台 operation_id 分开；公开错误/冲突归一化；支持配置未知时早拒绝 |
| 约束 | 不嵌入 SDK，不写私有 DB、不调用私有协议、不修 API；使用现有公开 runtime identity，不要求 B1；缺 pid/startTime 或观测冲突时仍不补造身份；暂不可自动证明的路径标 unknown |
| 验收 | 一个 Adapter 覆盖后续全部已承诺入口；CLI fallback 进入同一操作所有权；不兼容配置在创建或操作前被解释清楚；无 Host 任意路径/argv 注入 |
| 最有价值验证 | 在公开 mutation 已派发但响应丢失时，只观察原 runtime 名和收据，不创建替代对象；检查新官方版本对应字段是否仍一致 |
| 后续 | P1-W02/P1-W03/P1-W04/P1-W06/P1-W07/P1-W08 的所有 runtime 调用 |

### P1-W02 SQLite、身份、认证与操作受理

| 字段 | 内容 |
|---|---|
| 目标 | 首期 owner、多个 token 与简单 scope/对象权限、稳定 ID、幂等操作、可恢复受理记录、事件与 OpenAPI 起点 |
| 前置 | P1-W01 接口草案、中央 API/状态字段冻结；实现可与 P1-W01 并行 |
| 目录 | `internal/registry/`、`internal/auth/`、`internal/operations/`、`internal/api/`、`api/openapi.yaml`、`migrations/` |
| 上游复用 | SQLite WAL/FULL、官方在线备份能力；runtime 的生命周期锁仍由 runtime 自己拥有 |
| 具体任务 | 短事务受理 operation+幂等记录+事件；指纹含 principal scope、方法/动作、目标和业务参数；永久不重用 runtime 名；候选 generation 在 RPC 前按全部历史 MAX+1 预分配、允许空洞，失败候选不得重用；当前 machine generation 仅成功切换后更新；binding 持久 guest_management_status=ready/degraded/unknown 及当前 evidence；进度与最后观测；同 key 同请求返回原操作；If-Control-Epoch 与 revision 校验；control_epoch 正常重启不变、受控 DB 恢复才换新；token hash/撤销；列表、事件、task、snapshot、service 同样授权 |
| 约束 | 无企业多租户/RBAC/OIDC 硬依赖，无资源字节 reservation；不在 SQL 事务里等待 runtime；客户端断开不代表取消操作 |
| 验收 | 响应丢失后找回同 operation；不同指纹冲突；未认证、超 token scope 或访问未授权实例拒绝；UI 刷新可找回操作；单写者进程锁生效 |
| 最有价值验证 | SQLite 受理提交与响应之间杀 platformd，重新提交相同 key，只能得到原操作；权限过滤同时覆盖列表与事件 |
| 后续 | 全部业务；P1-W10 可立即实现登录/列表/操作 UI 和客户端基础 |

### P1-W03 统一生命周期、重启协调与基本准入

| 字段 | 内容 |
|---|---|
| 目标 | 串行协调 Create/Start/Stop/Pause/Resume/Delete 与后续分支/恢复计划；提供 EnsureRunning；启动先 reconcile |
| 前置 | P1-W01/P1-W02 |
| 目录 | `internal/lifecycle/`、`internal/lifecycle/reconcile/`、`internal/lifecycle/admission/`、`internal/metrics/` |
| 上游复用 | 原生生命周期与 locks、saved execution、状态/压力原因、capacity/metrics、现有 cgroup/balloon/admission |
| 具体任务 | 机器 mutation owner、多对象固定锁序；observed_state/desired_power/availability/observed_at 按规范分开；Resume 用原 pause ID；queued→dispatching→observing→succeeded/failed/recovery_required，调用前 FULL 提交意图；同机 wake 合并与 waiter 分离；超时转 observing 或 recovery_required，effect=unknown；启动核对公开 runtime 清单；默认全节点重型 start/resume/import/checkpoint 等操作并发 1 与 statfs 临界保护；一次复合操作持同一许可，内部步骤不嵌套等待同一许可；原生压力动作进入外部观测 |
| 约束 | 不追求公开接口之外的强 execution identity；不因 timeout 自动换名恢复；不按 stale route 判 running；不建 RAM/CoW 字节账；unknown 重型操作仍占排程位置，避免无限重复派发 |
| 验收 | platformd/serve 重启后可接管或明确 unknown；Resume/Delete 冲突序列清晰；binding_generation 只在替换 runtime 时增加，普通 pause/resume 不增加；旧响应不更新已替换绑定；frozen 不被普通访问偷偷继续 |
| 最有价值验证 | 原恢复调用超时后后台仍完成，同时重启 platformd 并有多个 wake：只能继续观察原恢复，不能启动第二份执行 |
| 后续 | P1-W04/P1-W06/P1-W07/P1-W08；P1-W09 在此基础上增加自动 idle 决策 |

### P1-W04 模板、内部工作区、原生 exec/files 与 WebTerminal

| 字段 | 内容 |
|---|---|
| 目标 | 用户能创建可长期工作的环境，使用内部磁盘、文件、普通命令和交互终端；准备独立 Task Manager 与 SSH 的模板入口 |
| 前置 | P1-W01/P1-W02，生命周期接 P1-W03 |
| 目录 | `guest/templates/managed/`、`internal/registry/templates/`、`internal/execbridge/`、`internal/filebridge/`、`internal/execbridge/terminal/`、`web/src/features/terminal/` |
| 上游复用 | 官方模板/镜像与内部 storage；native exec/files/PTY；Guest OpenSSH 包；模板内 runit supervision |
| 具体任务 | 固定 root 基础模板 digest、默认 /workspace；预装 taskd/taskctl/task-runner，native exec background:true 引导 flock 防重入的 runit 并握手；检测 Guest cgroup2 rw/freezer/kill；普通 Exec/Terminal/Task 默认 UID/GID 0，显式普通用户可选；原生 stdout/stderr/exit/PTY resize 与有界 fan-out；文件复用 native files，支持 Guest 系统路径，必要时使用固定无状态 helper、staging 与限额；root SSH 公钥登录、关闭密码；启用普通 virtio-net/strict egress。root 使用不设置 taint |
| 约束 | 无 Host mount/外部卷/Host socket；普通 exec 不宣传为持久任务；终端/文件预览不将 Guest 可执行 HTML 混入管理 origin；不接受任意 Host 文件路径 |
| 验收 | Web/CLI 真实终端和文件操作；重启后内部工作区存在；exec 非零退出与连接丢失区分；模板不带平台管理 token、Host 密钥或共享固定 Guest 身份；无法提供受管 task tree/freeze 能力时明确受影响功能门槛，不以 nohup 替代监督 |
| 最有价值验证 | root 安装软件、修改 Guest 系统文件、SSH/SFTP/Exec 正常；文件桥不能访问 Host 路径或其他 Guest；native session 断连真实展示，supervisor 不依赖它存活 |
| 后续 | P1-W05 Task Manager；P1-W08 SSH；P1-W10 完整终端和文件体验 |

### P1-W05 独立 Guest Task Manager 与持久任务闭环

| 字段 | 内容 |
|---|---|
| 目标 | 用户后台任务脱离 native exec 会话，具备持久 ID、状态、日志、退出、取消与重新连接；保持源 Agent 不变 |
| 前置 | P1-W02/P1-W03/P1-W04；不依赖 P1-W08，不开 Guest 公开 task TCP 端口 |
| 目录 | `cmd/sandbox-taskd/`、`cmd/sandbox-taskctl/`、`cmd/sandbox-task-runner/`、`internal/tasks/`、`internal/tasks/bridge/`、`guest/templates/managed/services/` |
| 上游复用 | native exec background:true 只启动自有 supervisor；后续 taskctl 短 exec 先经平台 EnsureRunning+hold，再以 autoStart:false 调用公开 /exec，连接 Guest UDS；native 文件接口没有该字段，单独以平台活动/生命周期门约束；内部持久磁盘、公开 exec/files [S2](#source-task) |
| 具体任务 | `/var/lib/sandbox-taskd` JSON intent/state 原子写+fsync+目录 fsync；task_namespace 内 start token/spec hash 去重；每任务 runner 与 payload 分离，payload 放专属 cgroup；runner 输出 segmented stdout/stderr、byte offsets/ranges，日志同步后再写 final receipt；taskd 重启核对 boot/PID/start/cgroup/runner，保留未知窗口；cgroup 取消整个 payload tree；任务 hold/suspect；Pause/Resume 原任务；复制屏障与继承记录 |
| 关键约束 | 不把 background PID 当收据；不凭 kill(pid,0) 判断归属；不要求 Guest SQLite；manager/runner 在 payload cgroup 外；默认 root payload，合作式 cgroup cancel/freeze；无法确认或 root 主动逃离时 unknown，不重启 argv |
| 完成验收 | CLI/SDK 退出后任务继续，重连能按游标续读；platformd/serve/taskd 重启不会重复启动已受理任务；真实 exit 或 unknown 均可解释；cancel 处理子孙进程；等待模型时持有 task hold；历史 task 不因 Fork/rollback 被重新提交 |
| 最有价值验证 | 在受理 intent 后、spawn/receipt 未完整落盘前杀 taskd，再重启并重试同 token；允许保留 unknown，但不能出现第二次业务写入；杀 taskd 不应杀独立 runner |
| 后续 | P1-W06 active recovery、P1-W07 Live Fork 子任务独立接管与 Clone 协调、P1-W09 holds、P1-W10 UI/SDK |

### P1-W06 快照目录、历史、导出导入与基本备份

| 字段 | 内容 |
|---|---|
| 目标 | 复用原生格式交付有历史、可导出导入、可独立读取的恢复点；区分消费性 pause 与独立备份 |
| 前置 | P1-W01–P1-W03；与 P1-W05 并行；包含 active task 的捕获必须等 P1-W05 freeze/恢复门通过 |
| 目录 | `internal/snapshots/`、`internal/artifacts/`、`internal/backups/`、`internal/imports/` |
| 上游复用 | 公开 checkpoint/export/restore、runtime 内部 CoW/增量层；默认 standalone/full-state，不手写 delta engine [S3](#source-checkpoint) |
| 具体任务 | 本地每 UUID 不复用的制品目录、SHA256/manifest/兼容信息；自有 capture 将 operation/源绑定/屏障/任务分类与工件 hash 作为耐久 provenance；导出不可变 manifest.json 原始字节并用平台 Ed25519 密钥生成 manifest.sig，签名绑定 runtime 文件 hash、task_content、guest_management_status 与 barrier/freeze 声明；导入须匹配本地 capture 证据或本机配置的可信 signer；staging→ready/quarantined→deleting→deleted；history 为平台 catalog；purpose=clone 使用 copy-ready clean 捕获；purpose=recovery 若有 owned active task，先确认 payload cgroup frozen、写 freeze/barrier manifest，再 capture；closure_verified_at 与 restore_verified_at 分开；导出全部依赖、导入限额/路径/可信来源；引用/pin 与删除门互斥 |
| 关键约束 | native pause artifact 可被 Resume 消费，不能充当 last_good backup；仅内部磁盘，不读私有 pause 目录、不删 runtime 层；默认 standalone 即完成要求；可选原生增量仅在公开闭包验证后启用，不成为产品完成前置；active freeze 无法证明时只支持 clean snapshot，不能假装 active safe；SHA 只证明完整性，导入文件自称 clean/frozen 不可当事实；未知来源/签名不可信置 quarantined，禁止自动 Restore 与启动式 verify；签名私钥只在宿主受保护 secret store，公钥信任仅本机配置，导出不得自授信任 |
| 完成验收 | 快照列表/历史/导出/导入是真实 API；损坏或伪造 clean/frozen 声明隔离且不启动；可信 provenance/签名与 clean/active/unknown 清楚；源只在捕获结束已确认后解冻；一份独立导出可在不访问源路径/缓存的兼容数据目录恢复 |
| 最有价值验证 | 源 task payload 冻结后捕获，在受控恢复副本首次执行时确认仍冻结；默认在冻结下 kill→确认空→写 interrupted；若此门失败，只禁用 active recovery，不影响 clean 快照/Clone；同一导入用例中篡改 manifest 后重算普通 SHA 仍必须验签失败、不能在线 verify |
| 后续 | P1-W07 Independent Clone/rollback、P1-W11 备份恢复与保留 |

### P1-W07 Live Fork、Independent Clone 与稳定身份回滚

| 字段 | 内容 |
|---|---|
| 目标 | 提供两种准确命名的复制方式与 restore/rollback；展示原生父依赖，保持稳定 logical machine 身份 |
| 前置 | P1-W03；P1-W05 用于完善已复制 Task 的归属与接管（不作为原生 Fork 的运行时准入）；Independent Clone/restore/rollback 再依赖 P1-W06 |
| 目录 | `internal/copies/`、`internal/copies/restore/`、`internal/lifecycle/plans/`；复用 snapshot/task/auth 模块 |
| 上游复用 | native Branch 与父 pause/delete guards；checkpoint 流导入 created，再通过公开 start 启动；公开 set ports/network [S3](#source-checkpoint)、[S4](#source-branch) |
| 具体任务 | Live Fork 仅提供原生实时复制语义，鉴权/原生 profile/容量/生命周期冲突检查后直接 native Branch；不关闭活跃任务、会话、服务，不要求 GTM 健康、非 root、clean 或应用安全点；不实现或暴露任何可选安全点、forkpoint、hold/waitReady 或相关等待/协调逻辑。预分配 child name/ID；子端口 remint 仍为 loopback，按子配置核实；新 machine/task namespace，子内 `adopt_live_fork` 接管已经复制的 runner/进程/日志、分配新 task ID 和 origin 关联，已终态历史只读，证据不足 unknown，绝不重发 argv；核实新 SSH identity/服务再发布 Gateway。Independent Clone 保留可信来源 clean standalone checkpoint，导入 created 后清 ports/限制出站再 Start，验证独立闭包 |
| 关键约束 | Branch 后清 ports 不是首次指令前 fence；风险提示不阻塞 Fork。Guest root 不触发永久失效；GTM 接管异常只降级任务功能，不能删除子或暂停父。父子资源依赖保留、无原地 detach 或手删共享层；结果未知按原 runtime 名 reconcile。Clean Clone/recovery 仍用合作式协调与相应证据，不把此要求用于 Fork |
| 回滚任务契约 | 按新鲜旧状态分支：running→公开 Stop 并确认 stopped 与 restart.policy=never；created/stopped 核对无 saved execution/未决启动/自动重启意图且有效策略为 never；paused saved 不能 Stop，必须请求显式 old_state_policy=export_then_delete_on_paused，先公开导出当前 pause 工件、校验并 pin，再 DELETE cascade=false 并确认旧名 absent，之后才可 candidate Start；pressurePaused 无 PID 不代表 fence，native guard 可自行 Resume。任一步 unknown/依赖拒绝就阻塞；paused 分支只留导出制品，不保留可重新启动的旧 runtime。active recovery 仅同 logical machine、可信任务/freeze manifest；默认 cancel_captured，显式 resume_captured 才重新绑定后解冻，不 spawn argv；新启动 unknown 不重启旧机补偿 [S8](#source-rollback) |
| 完成验收 | 活跃 root 进程/Task 及打开的 SSH/Exec 存在时 Live Fork 成功，父子原进程各自推进、分支 ID 与任务 ID 独立、日志/取消互不影响、不重跑 argv；GTM 缺失或未知不阻原生 Fork；父保护可见。独立副本源资源不可用后仍可恢复；rollback 仅在新绑定 ready 后切换稳定入口 |
| 最有价值验证 | 一个运行中 root 持久计数 Task 和未登记后台进程，保持 SSH/Exec 连接发起 Live Fork；验证父子继承非零计数、随后写入隔离、新 Task ID/origin、无 argv 重启、子取消不影响父、父连接无强制关闭；确认未调用 PrepareCopy 或应用安全点协议，API/CLI/SDK 不提供相关选项；模拟接管异常仍保留 child。一次 clean Independent Clone 后验证源删除/不可访问下的 RAM/文件/cold start/再次恢复；同一恢复用例覆盖旧 paused 导出+pin→Delete absent 后才 Start；核实端口和 SSH identity |
| 后续 | P1-W09 parent blocker 与 idle、P1-W10 分支/历史/rollback UI、P1-W11 恢复演练 |

### P1-W08 稳定 Gateway、HTTP/TCP/SSH 与服务域名

| 字段 | 内容 |
|---|---|
| 目标 | 统一认证后访问稳定逻辑服务，必要时调用 EnsureRunning，再使用当前 backend；与 VM 运行状态解耦 |
| 前置 | P1-W02/P1-W03/P1-W04；可与 P1-W05–P1-W07 并行，不依赖自动 idle 已完成 |
| 目录 | `internal/gateway/`、`internal/gateway/services/`、`internal/gateway/tunnels/`、`internal/gateway/sshconfig/`、`cmd/sandboxctl/` |
| 上游复用 | 普通 virtio-net loopback ports、Go HTTP/TLS/WebSocket/TCP、Guest OpenSSH；可选 Caddy、现有 wildcard DNS |
| 具体任务 | HTTP/SNI/Host 校验与目标授权；反代 body 背压/大小限制；forwarding 三态及可信错误；鉴权 WebSocket tunnel；SSH ProxyCommand；显式配置的稳定 mTLS TCP listener（认证先于 wake）；每连接 hold；ready/epoch 检查；端口集合统一管理；Token/证书撤销；连接数/等待预算 |
| 约束 | runtime UDS 与高端口不公开；runtime ports 127.0.0.1，SMOLVM_EGRESS_FLOOR=strict；不允许请求任意 Host dial；不以 DNS 差异路由同 port raw TCP；首字节可能发送前改 may_have_started，库和 SDK 不得透明重放；使用受控不重试 RoundTripper，不能只设置 DisableKeepAlives 就假定不会重试；无 CoreDNS/Host sshd 必需服务 |
| 验收 | 稳定 HTTP URL 与 SSH config 跨 Pause/Resume、rollback；授权失败不 wake；跨对象拒绝；TCP half-close 与首包保留；WebSocket/SFTP 可实际工作；Guest 与公网无法绕过 Gateway 访问其他 backend |
| 最有价值验证 | 多个 HTTP/SSH/TCP 客户端同时访问 paused 实例，第一 caller 退出而其他继续；只恢复一次，向应用提交的计数 POST 至多一次，错误后 SDK 不重发 |
| 后续 | P1-W09 自动 idle；P1-W10 服务管理/SSH 配置/SDK tunnel helpers |

### P1-W09 活动模型、空闲休眠与访问唤醒整合

| 字段 | 内容 |
|---|---|
| 目标 | 让任务/连接/显式租约保护正常工作，确实空闲时自动 Pause；新活动与 Pause 同时发生也可解释 |
| 前置 | P1-W03 EnsureRunning、P1-W05 Task 状态、P1-W08 连接 hold；P1-W07 提供依赖 blocker |
| 目录 | `internal/idle/activity/`、`internal/idle/`、`internal/lifecycle/`；不新建服务 |
| 上游复用 | 原生 pause/resume 与压力原因；现有 native guard、balloon、scope 保持原职责 |
| 具体任务 | 活动 revision、task/connection/exec/keep-awake hold；suspect 阻 idle；idle 最终检查与 pause owner 原子裁决；saving 期间新请求排 wake_pending；managed 模板活动探针通过后默认 30min，custom 默认 idle off，user keep_running 永久 hold；lease 过期转 suspect，owner 证实/管理员释放才解除；UI/metrics/控制 RPC 不刷新活动；父依赖解释；重启重建任务 hold，失联连接不伪装仍在转发；压力事件与正常 idle 分类 |
| 约束 | CPU 低不是唯一闲置信号；无 B6 bridge；不把 heartbeat TTL 到期直接当任务结束；unknown 不能自动重跑或删唯一恢复点；不承诺活 TCP 会话跨 full-state 存活 |
| 验收 | 等模型的真实任务、长 SSH/WS/TCP 不被正常 idle；结束后按设置保存；并发请求恰落在 pause 最终检查前后均有明确结果；故障重启后保活原因准确且可诊断 |
| 最有价值验证 | 同时触发 idle 最终检查、任务开始和 HTTP wake，反复交错后任务不重复、不被错误 pause，至多一条 pause/resume 序列；紧急压力时正确标原因而不宣称 hold 失效即安全漏洞 |
| 后续 | P1-W10 设置/hold 解释/状态展示，P1-W11 整体演练 |

### P1-W10 完整 Web、CLI、Python 与 TypeScript 产品面

| 字段 | 内容 |
|---|---|
| 目标 | 第一阶段交付所有要求的真实用户操作面，既能日常交互，也能由 agent 长期管理任务与环境 |
| 前置 | P1-W02 开始；各业务 API 逐包接入；不是等 P1-W09 结束才动工 |
| 目录 | `web/`（React/TypeScript/Vite/xterm.js）、`cmd/sandboxctl/`、`sdk/python/`、`sdk/typescript/`、`examples/`、`api/` |
| 上游复用 | OpenAPI 生成基础 types/client、Python httpx、TypeScript fetch/WebSocket、Cobra CLI、平台 API/SSE/WebSocket；不把官方 smol CloudTransport 当 serve SDK |
| 具体任务 | 列表/详情/创建/配置/生命周期；分支树/依赖与历史；资源/压力；真实终端/文件；任务日志/取消/未知；服务端口/域名/SSH；idle/holds；操作进度/错误/恢复入口；SSH key/client certificate 管理、模板与系统设置；rollback UI/CLI 明示 paused 旧实例将被导出后删除、只保留制品，不暗示可直接启动旧实例；CLI exit/JSON/等待；两语言 SDK wait/reconnect/cursor/幂等 helper 与可运行例子 |
| 约束 | UI 状态来自后端，不乐观伪造 running/成功；资源设置不能显示未实现高级配额；客户端重连只查询原操作/任务；无 E2B 兼容层；不缓存 private runtime endpoint |
| 验收 | 同一完整用户流程可由 Web、CLI 和两语言 SDK 完成其相应功能；刷新后找到原操作；日志 gap/unknown/recovery_required 可理解；SDK 请求重试不会重复业务任务；有真实示例而非只生成包 |
| 最有价值验证 | 端到端 agent 脚本创建环境、启动长任务、断开、重连续读、查询退出、checkpoint/clone、连接服务；同时浏览器刷新与 token 过期不触发重复 mutation |
| 后续 | P1-W11 发布与用户验收；各包按真实页面暴露的缺口修复 |

### P1-W11 安装、更新、备份恢复、诊断与发布收口

| 字段 | 内容 |
|---|---|
| 目标 | 普通用户能在目标 PVE Linux 节点安装并长期运维；控制面故障、runtime 升级和备份恢复有可执行流程 |
| 前置 | 安装骨架从 P1-W01 开始；最终验收依赖 P1-W03–P1-W10 |
| 目录 | `packaging/systemd/`、`packaging/install/`、`internal/diagnostics/`、`internal/maintenance/`、`tests/integration/`、`tests/scenarios/` |
| 上游复用 | 官方 release 与 checksum、现有 scope/cgroup、journald、SQLite backup、runtime version/capacity、已验证原 PVE 脚本 |
| 具体任务 | 用户已允许 root：platformd 使用专用非 root 账户，官方 serve 以 root 启用原生 systemd scopes；serve 重建 0600 UDS 后受控 root-owned ExecStartPost helper 验证 owner/类型/固定路径再授予唯一平台控制组 0660、父目录 0770；完整 runtime API 本身是宿主特权边界 [S7](#source-deployment)、配置检查、安装/升级/卸载保留数据；manifest pin；systemd 真重启验证；SQLite 在线 DB 备份/前向迁移，受控 DB restore 维护模式与 control_epoch 换新；制品包复制到不同故障域；恢复隔离与确认；日志轮换/健康/metrics/诊断包脱敏；release note 与具体已知限制；合并验收记录 |
| 约束 | 不把 KillMode=process 当通用答案；不自动销毁未知 VMM；无私有 patch queue/常驻 Redis/K8s/CoreDNS；低空间不删除唯一 last-good；旧 DB 备份恢复不能自动重放其“未完成”请求 |
| 验收 | 干净安装可走完整产品流程；platformd/serve systemctl restart 与 SIGKILL 的差异有实测结论；升级前备份可回退或明确拒绝不兼容回退；卸载默认留数据；诊断包含版本/操作/状态/压力但无 secret/RAM |
| 最有价值验证 | 在隔离数据目录执行一次安装→长任务→Pause/访问 wake→独立复制→DB/制品备份→platformd/serve 重启→恢复→版本升级/失败回退的真实流程，故障只注入关键边界 |
| 后续 | 第一阶段发布；剩余特定能力缺口进入明确第二阶段列表，不扩成新生态研究 |


<a id="completion"></a>
## 6. 完成记录、集成与发布门

每个工作包提交一份简短实际记录：实现 commit/依赖版本、已经工作的 API/CLI/UI 路径、最有价值测试输入与结果、原始错误和仍阻塞的准确范围。记录进入代码仓库正常 PR/CI 产物，不创建新的规划文档树，不写与实现逐行同构的重复测试。文档、OpenAPI 和真实行为不一致时先修正契约或实现，不能把 UI 的绿色状态当证据。

第一阶段发布前必须通过 [验收矩阵](ACCEPTANCE_AND_OPERATIONS.md#acceptance-matrix) 与 [五组窄门](ACCEPTANCE_AND_OPERATIONS.md#targeted-gates)。有特定 gate 失败时，按规范禁用对应能力并清楚展示；用户要求的持久任务、完整产品面和基本状态管理仍是总体完成条件，不能把缺失页/假按钮标完成。active-task recovery 的 freezer 门失败时保留 clean 快照/Clone，明确 active 恢复不支持；不为这条增强路径修改 Guest kernel。

明确不在第一阶段：Named/稳定 Guest 私网 IP、私网互联、外部卷/共享存储、原地零复制 detach、复杂资源账本/CoW 预测/精确配额/自有 OOM、Kubernetes/Redis/PG/Raft、多节点、macOS/HVF、E2B 兼容、企业 RBAC/计费。将其保持为简短后续方向，不提前实现空框架，也不让这些方向成为首期安装依赖。

<a id="sources"></a>
## 7. 影响工作拆分的固定源码依据

<a id="source-runtime"></a>
**S1：公开身份与字段已经存在。** [MachineInfo](https://github.com/smol-machines/smolvm/blob/9d3ab4c82756456565544558ac2ea5412afa84ef/src/api/types.rs#L1513-L1648) 与 [RuntimeIdentity](https://github.com/smol-machines/smolvm/blob/9d3ab4c82756456565544558ac2ea5412afa84ef/src/agent/live_resize.rs#L14-L35) 包含 pid/startTime/可选 bootId；B1 不应成为首期前置。仍要检查真实观测的新鲜度与缺失情况。

<a id="source-task"></a>
**S2：background 是引导入口，不是持久任务完成证明。** [ExecRequest background](https://github.com/smol-machines/smolvm/blob/9d3ab4c82756456565544558ac2ea5412afa84ef/src/api/types.rs#L287-L297)、[exec handler](https://github.com/smol-machines/smolvm/blob/9d3ab4c82756456565544558ac2ea5412afa84ef/src/api/handlers/exec.rs#L117-L166)。其独立监督、日志/退出与复制归属由自有 Task Manager 实现，原生 session 行为不被重新命名为持久性。

<a id="source-checkpoint"></a>
**S3：导入 created 与端口继承必须分开处理。** [Restore 查询](https://github.com/smol-machines/smolvm/blob/9d3ab4c82756456565544558ac2ea5412afa84ef/src/api/handlers/machines.rs#L1692-L1725)、[导入注册为 created](https://github.com/smol-machines/smolvm/blob/9d3ab4c82756456565544558ac2ea5412afa84ef/src/api/handlers/machines.rs#L3569-L3604)、[restore 空 ports 继承](https://github.com/smol-machines/smolvm/blob/9d3ab4c82756456565544558ac2ea5412afa84ef/src/machine_spec.rs#L1440-L1478)、[公开 set ports 真正清空](https://github.com/smol-machines/smolvm/blob/9d3ab4c82756456565544558ac2ea5412afa84ef/src/agent/live_ports.rs#L12-L64)。restore query 使用 snake_case，与 create/branch body 的 camelCase 不同。

<a id="source-branch"></a>
**S4：Branch 不给任意首次执行前 fence，父依赖保护不可绕过。** [Branch ports 空值语义](https://github.com/smol-machines/smolvm/blob/9d3ab4c82756456565544558ac2ea5412afa84ef/src/api/types.rs#L2113-L2117)、[pause source guard](https://github.com/smol-machines/smolvm/blob/9d3ab4c82756456565544558ac2ea5412afa84ef/src/portable_checkpoint.rs#L2308-L2315)、[delete dependency guard](https://github.com/smol-machines/smolvm/blob/9d3ab4c82756456565544558ac2ea5412afa84ef/src/api/handlers/machines.rs#L6597-L6616)。Live Fork 必须接受原生图，Independent Clone 则新建实例并验证独立。

**S5：live 网络更新不等于通用断网 fence。** [Network handler](https://github.com/smol-machines/smolvm/blob/9d3ab4c82756456565544558ac2ea5412afa84ef/src/api/handlers/machines.rs#L7764-L7851) 与 [网络配置规范化](https://github.com/smol-machines/smolvm/blob/9d3ab4c82756456565544558ac2ea5412afa84ef/src/config.rs#L1833-L1864)。默认模板从出生即使用 strict floor 下的显式 allowlist；恢复前限制用公开 import 参数，不能拿 live 空数组冒充断掉现有 TCP。

**S6：hold 与压力保护属于不同层。** [memory pressure guard](https://github.com/smol-machines/smolvm/blob/9d3ab4c82756456565544558ac2ea5412afa84ef/src/api/memory_pressure.rs#L1-L54)、[victim/candidate](https://github.com/smol-machines/smolvm/blob/9d3ab4c82756456565544558ac2ea5412afa84ef/src/api/memory_pressure.rs#L187-L225)。平台不修改其判定；紧急 pause/kill 与正常 idle 分开展示。

<a id="source-deployment"></a>
**S7：root serve 是当前原生 scope 的条件。** [is_available](https://github.com/smol-machines/smolvm/blob/9d3ab4c82756456565544558ac2ea5412afa84ef/src/systemd_scope.rs#L421-L450) 明确要求 root/systemd/busctl；[UDS 创建](https://github.com/smol-machines/smolvm/blob/9d3ab4c82756456565544558ac2ea5412afa84ef/src/cli/serve.rs#L1047-L1074) 发布前设置 0600，需自有受控授权 helper。platformd 非 root 不表示能调用完整 runtime API 的进程处于强宿主隔离。实际 unit/cgroup 重启验收仍为必需。

<a id="source-rollback"></a>
**S8：paused 不能用无 PID 或 Stop 409 充当旧执行 fence。** [原生 Stop 的 saved-execution 拒绝](https://github.com/smol-machines/smolvm/blob/9d3ab4c82756456565544558ac2ea5412afa84ef/src/api/handlers/machines.rs#L6180-L6225)、[pressure guard 自动 Resume](https://github.com/smol-machines/smolvm/blob/9d3ab4c82756456565544558ac2ea5412afa84ef/src/api/memory_pressure.rs#L1-L19)、[Delete 的依赖保护](https://github.com/smol-machines/smolvm/blob/9d3ab4c82756456565544558ac2ea5412afa84ef/src/api/handlers/machines.rs#L6576-L6649)。paused 回滚使用公开导出和非 cascade Delete，确认 absent 后才启动候选，不读私有保存态或绕过依赖。
