# 2026-11-27 生产验收与发布收尾

交接时间为 2026-11-26。明天先锁定一个生产候选，在生产同配置的隔离环境完整跑通，再在授权窗口使用专用账号和临时资源从外网走一次生产验收。多节点故障演练和大并发压测使用独立环境。以下是待执行计划和已确认的本机证据，今天未部署生产。

11 月 28 日已完成内核 CoW、父机模型流和恢复修复的集成验收。新的源码起点、实验 Bundle、日志和工作目录见[合入交接](kernel-cow-closeout-2026-11-28.md)。下面标注旧 SHA 的发布测试仍按各自版本追溯。

## 已确认的代码与分支

| 交付线 | 已验证版本 | 明天的收尾 |
|---|---|---|
| 平台、CLI 和控制台 | `polish/integration@4e1ba470ed66`，schema 1019。此提交通过 Go 全量、宿主原生 OpenSSH、Vite、Darwin arm64/amd64 CLI 构建。最后一轮真实 SSH/HTTPS KVM 验收绑定 `0f35c613d14d`。更早的 J01–J10 与 J11 各有记录，以其各自 SHA 为准。 | 以当前集成分支为起点，生成一套包含平台、CLI 和控制台的冻结制品。用最终候选补同版 J01–J11 核心链路。确认发布目标后合并，保留源提交、版本、构建和回退凭据。 |
| Runtime / Guest | `polish/runtime-candidate@cb31d6c`，测量 Bundle `sha256:3de1c4f1a848edd37b8bc5c94b944e0dd5c18ab3d90874a1348ecc0529710a3c`。同期还留有 `polish/native-files@2b172e8`、`polish/runtime-glue@b724f99`。 | 按发布源和最终候选比较切片差异，核对实测文件权限、Agent 版本、Runtime 及 reader。确认成果和证据已被候选包含后再收源分支和工作树。 |
| 多节点 | `feat/multi-node-v1@1c039e3791f1` 已合入平台基线。早期发布包源码为 `53fefd0`，schema 1025。同一台主机真实跑过一个平台和两个回环 mTLS Node，N01–N18 的 41 个具名测试通过。 | 在独立双 Worker 部署验收 1019→1025、跨主机访问和兼容回退，然后验证规模化负载。 |
| 内核 CoW | 已合入平台和 Runtime 候选。libkrun `b59c9bc8`，smolvm `e80e2579`。已在隔离 Worker 验真实磁盘、网络、anchor 生命周期和跨启动恢复。[精确版本与证据](kernel-cow-closeout-2026-11-28.md) | 为专用 Worker 冻结发行制品，验安装、重启、实际能力和回退。已有脚本默认使用 Native Live Fork。 |
| 已回收开发分支 | 11 月 26 日回收 31 个工作树并删除 10 个已合并本地分支，`archive/polish-closeout/` 保留 7 个开发切片或诊断引用。前后端已在集成提交中。 | 从 11 月 28 日交接指定的工作目录开始，旧证据按[合入记录](kernel-cow-closeout-2026-11-28.md)追溯。 |

[平台验收记录](/home/dev/workspace/deepfork-smolvm/platform-smolvm/.git/polish-coordination/integration-status.md) · [SSH / HTTPS 实测清单](/home/dev/workspace/deepfork-smolvm/.build/polish-20261126/final-0f35c613d14d/managed-access-verification.json) · [分支归档清单](/home/dev/workspace/deepfork-smolvm/.build/polish-20261126/git-closeout/branches.tsv) · [多节点实现和验收](multi-node-v1.md) · [内核验证](/home/dev/workspace/deepfork-smolvm/kernel-cow-k1/linux-patches/evidence/anchor-exit-2026-11-26/README.md)

## 1. 上午先冻结候选和生产验证条件

- [ ] 记录实际环境的版本、schema、control epoch、Bundle/reader、模板摘要、Worker/KVM、磁盘和有效内存预算。核对控制台 origin、独立的 apps 域名、通配 DNS/TLS 及外部 HTTPS。锁定候选 SHA、制品 SHA-256、签名和下载 URL，保留对应的旧包。
- [ ] 备份 SQLite、配置和可恢复材料，检查备份，并在隔离副本恢复一次。确认旧版本能否读候选 schema、维护状态和回退触发条件。涉及 schema 回退时使用相容备份与匹配二进制，验证新的 control epoch 能挡住旧请求。
- [ ] 准备专用测试部署或获准的测试资源范围、测试账号、不同范围的凭据、资源清单、可观察的后端服务、外网客户端和变更窗口。升级、断电和压测只作用于获准的测试机器与节点。保留用户工作负载及 PVE 管理网络。有阻塞先标出缺少的主机、权限、证书或时间窗口。复用开发机时，构建按 `heavy.lock` 排队，KVM 和压力实验先拿该锁，再拿 `worker-lab.lock`。

[安装、备份与回退](../../packaging/install/README.md) · [现场预检与准入](ACCEPTANCE_AND_OPERATIONS.md#6-部署隔离与pve边界)

## 2. 一条生产端到端工作流

| 顺序 | 必跑检查 | 验收证据 |
|---|---|---|
| 登录、下载、建机 | 外网登录 Web，签发最小范围的 CLI token，从部署后的下载入口获取已签名或有可信校验清单的客户端。使用新配置目录登录、建机、查询、改名。另测 read/tunnel 凭据、过期和撤销。 | 构建版本、下载摘要、operation ID、授权结果；生产请求使用真实 API。 |
| 连接、文件和服务 | 终端、PTY resize、远程退出码、无感 SSH 和旧密钥；SFTP 和平台 API 传输分别校验字节。测试 64 MiB 上传边界、取消、回执丢失和执行换代。发布私有 HTTPS，测试登录跳转、凭据与浏览器票据、真实就绪和慢请求期间的活动保护。 | 文件 SHA-256、请求 ID、绑定和 execution epoch、授权及服务观察；不确定写入只查原意图，HTTP POST 只执行一次。 |
| Fork 和保存点 | 源运行时做独立 Live Fork，核对内存、根盘和数据卷；做批量部分成功、对失败子项重试。创建 VM 保存点，修改源配置，再验证同机回滚、从点建机、卷冲突策略和旧点可读。 | 父子读写隔离、capture ID、原 operation/child/attempt、捕获配置与恢复观察。 |
| 生命周期和清理 | Standby、Hibernate、Wake、冷 Stop/Start；覆盖默认策略、实例覆盖、运行租约及模型等待。被动状态、metrics、inventory 不触发唤醒；核对指标范围、采样时间、不可用字段和执行身份。删除本次创建的机器、卷、服务、点和凭据。 | 权限/唤醒事件、Lease 与真实 Worker 观察；清理前后资源、quota 和 reservation 对账。 |

主流程通过后再在隔离部署演练重启、升级和恢复。验收记录包含版本与制品哈希、步骤、日志、错误码、重试及可定位的失败原因。每项标记通过、失败或未验证。仅 UI 可见或 API 受理不足以证明 Guest 或服务已就绪。

[J01–J10 联合检查](/home/dev/workspace/deepfork-smolvm/platform-smolvm/docs/smolvm-phase2-polish-2026-11-26/PARALLEL_EXECUTION.md:141) · [J11 CLI 定义](/home/dev/workspace/deepfork-smolvm/platform-smolvm/docs/smolvm-phase2-polish-2026-11-26/02_backend/BACKEND_RESEARCH_AND_PLAN.md:361) · [CLI 命令与示例](cli-workflows.md)

## 3. macOS、CLI、外观与发布入口

| 工作 | 当前状态 | 明天的验收 |
|---|---|---|
| 从部署站点下载 CLI / Mac | 现有 Linux 服务端安装包包含 `sandboxctl`，已生成 Darwin CLI 构建。源码中未找到面向用户的客户端发布下载入口。 | 在管理站点提供清楚的 Download 入口，按候选支持范围链接 Linux CLI、macOS arm64/amd64 CLI 和 macOS App。每个资产提供不可变版本、可信校验清单或签名、最短安装说明。用无预装、无缓存的外网客户端验证下载、版本、首次安装、升级、旧版本链接及失败提示。 |
| macOS App 和 CLI | 已有引导、REPL、动画和菜单栏源码；Darwin 双架构 CLI 已编译通过。AppKit 真机操作、签名与公证仍待验证。 | 用 Apple Silicon 和 Intel 真机或明确记录实际覆盖，在干净账号验证首次启动、退出重开、切换 profile、创建、终端与 SSH 别名、Web URL、睡眠与唤醒。CLI 覆盖 PATH、zsh、非 TTY、`--json`、脚本退出码、终端缩放、跳过动画、减少动态。用真实发布产物验证 helper、Developer ID 签名、`notarytool` 公证、staple 和 Gatekeeper。 |
| 外观替换 | 原型通过 `SANDBOXCTL_BRAND_DIR` 读取可替换的 terminal 字样和菜单图标。 | 验收自有品牌的 CLI 字样与动画、终端静态版、浅色/深色菜单图标、App 图标和控制台登录/下载页。截取正常、窄终端和减少动态三组证据，核对公开包的素材来源和授权。 |

Mac 真机、Developer ID 与公证凭据未就绪时，可以验证内部包，并把公开下载发布标记为待验证。CLI 与 App 是否必须同批对外发布，以签名和干净设备验收结果决定。

明天单开一轮 [外观与首次使用体验验收](boxd-visual-handoff-2026-11-27.md)。先定自有 Web/CLI/Mac 标识和主要页面层级，再从干净账户记录下载、登录、首台机器、首次连接。历史 Chromium 和 PTY 样本用于对照，签名 Mac 包的图标、菜单状态和可访问性从真机留证。

[macOS 构建和品牌入口](../../packaging/macos/README.md) · [首次启动样例](/home/dev/workspace/deepfork-smolvm/.build/polish-20261126/cli-onboarding-macos/final-5c153be/prototype/README.md) · [Apple 公证文档](https://developer.apple.com/documentation/security/notarizing-macos-software-before-distribution)

## 4. 浏览器 OAuth / OIDC 登录

当前 Web 用账号密码和会话。CLI 交互引导可以打开 API Keys 页面，用户仍需粘贴平台 token，菜单栏复用该 CLI profile。当前候选未发现已接入的人类用户 OAuth/OIDC 登录链路。明天将现有 token 下载登录路线先验收，再用独立任务验证 OAuth/OIDC。

- [ ] 确认身份提供方、测试应用、生产回调与账号准入规则。验证提供方身份到平台 principal 与 scope 的映射，指定账号绑定和权限变更的预期。OAuth 提供授权，用于登录时验证 OIDC 身份或该提供方的正式身份接口。
- [ ] Web、CLI 和菜单栏复用同一授权模型。CLI/Mac 使用系统浏览器和 Authorization Code + PKCE；验证回调严格匹配、state、一次性兑换。OIDC 按 issuer、audience、nonce 和签名校验 ID token。无浏览器客户端仅在提供方支持时引入 Device Flow；确认现有 `--token-stdin` 仍供自动化使用。
- [ ] 在独立应用注册上验收成功、取消、超时、错误回调、旧 code 重放、账号切换、登出、撤销、权限变化及并发登录。确认 token 与 secret 在 URL、CLI 输出、遥测和日志中均不会外泄。明确身份提供方登出与平台会话撤销的关系。

[现有 Web 登录路由](../../internal/platformapi/server.go) · [CLI 交互登录](../../cmd/sandboxctl/interactive_login.go) · [原生应用 OAuth RFC 8252](https://www.rfc-editor.org/rfc/rfc8252.html) · [OAuth 安全最佳实践 RFC 9700](https://www.rfc-editor.org/rfc/rfc9700.html)

## 5. 双物理 Worker、故障和高并发

**双机验收先于容量压测。** 准备两台独立可访问 KVM 的 Worker、一个平台、可回收磁盘或卷以及单独的 mTLS 证书。登记时核实不同的 worker、journal 和全局 runtime 身份，记录精确 Bundle、reader、模板和真实预算。两台分别做 pinned 放置，再测 auto placement 的筛选与拒绝原因。跨主机访问 Guest/PTY/SFTP/HTTPS，尤其核实托管 SSH 经远程 Node 的接线；测冷盘预热、Hibernate/Wake 和闭包备份，按已有手工恢复合同演练独立恢复目标。当前能力边界为远程 managed System、固定 owner 和 `known_only`。已断链的实例不能自动换节点重放，证据不足的节点仍阻止退役。

双机正向通过后，先单 Worker 短断链，再执行获准的重启、换证、日志丢失隔离、cordon/drain 和控制端恢复。检查 Unknown/reservation 状态，验证同一操作仅向原 Node 对账、回执丢失时不重跑，阻塞原因可见。

**压测分三组，各自从小档升阶。** 先记录空载基线。API 和活跃访问可从 1、10、50、100 并发开始，稳定后再试 200、500 或目标规模。这是负载生成档位，VM 创建/Fork 数以现场 Worker 的实际 RAM、盘、I/O 和准入预算核定。每一档记录并发连接、请求率、持续时间、p50/p95/p99、错误码、队列深度、Worker 预算/心跳、CPU、内存与 PSI、SQLite 写等待、磁盘吞吐以及清理后的残留资源。

| 压测组 | 混合负载 | 硬停条件 |
|---|---|---|
| 控制面 | 机器列表、metrics、operation wait、服务观察和少量生命周期写入 | 被动查询唤醒实例、状态与资源记账冲突、持续积压。 |
| 连接与数据面 | SSH/PTY、长连接、双向文件、HTTPS GET/POST、慢上游和唤醒风暴 | 越权、HTTP 写被重放、心跳误判、现有活动或租约被错误回收。 |
| VM 与存储 | 建机、Live Fork、并行捕获与恢复、卷、备份、不同负载下的节流 | OOM、最低磁盘保留或有效内存预算触线、数据哈希错误、Unknown 请求被再次执行或释放过早。 |

先定该环境的目标值和停止阈值。压力上升时先限制新增请求，接通清理和对账后再开下一档。报告区分首个退化档、长期稳定档和保护性拒绝。若两台物理 Worker 尚未到位，记录真实跨主机和大规模压力待验证，继续做单 Worker 的基准、制品下载和 Mac 测试。

[多节点 N01–N18](../../../multi-node/docs/deep-runtime/multi-node-v1.md) · [多节点登记与换证](../../../multi-node/docs/deep-runtime/WORKER-ENROLLMENT.md) · [多节点开发计划](/home/dev/workspace/deepfork-smolvm/platform-smolvm/docs/smolvm-phase2-polish-2026-11-26/04_multi_node/reference/MULTI_NODE_IMPLEMENTATION_PLAN.md) · [压力准入和恢复](ACCEPTANCE_AND_OPERATIONS.md#123-故障和压力操作)

## 6. Live Fork 性能、Worker 内核与 Agent 接续

### 性能基线

在开发机的 AMD 嵌套 KVM 上，用常规内核和 1 vCPU、512 MiB 的轻量 VM 运行两轮试验，每轮三次单子机 `smolvm machine branch`。内存里的标记在子机首次 `exec` 可读，最后清空了测试资源。

| 本地计时口径 | 样本范围 | 中位数 |
|---|---:|---:|
| 源机 RAM 捕获内部阶段 | 24–39 ms | 35 ms |
| 从 RAM 捕获开始到子机 VMM 完成 vCPU 恢复 | 136–170 ms | 155 ms |
| 从 CLI 启动到子机 VMM 完成 vCPU 恢复 | 232–312 ms | 304 ms |
| 从 CLI 启动到 `branch` 返回 | 394–506 ms | 441 ms |
| 从 CLI 启动到子机首次 `exec` 读到继承标记 | 464–579 ms | 516 ms |

136–170 ms 包含捕获开始后的本地子机准备和恢复，源机准备阶段更早发生。历史受管环境约 13 秒的数据来自 512 MiB、两块可写盘的另一条流程。Boxd 官方示例打印 `boot: 160ms`，技术文章描述运行中 VM 到子机恢复运行。字段的精确起止点、统计分位和公网可用边界未公布。我们的 Boxd 云端样本中，同一次 Fork 打印 `boot=1.3s`，从 Mac 计时为 3.57 秒。

明天在固定候选、同一源负载下，同时记录请求受理、源 vCPU 停顿、子 vCPU 恢复、首次 Guest `exec`、首次 SSH 和 HTTPS 业务响应。用子机身份与继承值核对 HTTP 响应，记录是否走热连接。优先复测受管 Native 路径，单独量化 Worker 轮询、盘切层和 Guest 身份激活。现有启动派发每 600 ms 扫一次，评估事件触发后再谈亚 200 ms 的受管目标。小样本列实际范围，规模上来后再报告分位。

[本地 CLI 样本](/home/dev/workspace/deepfork-smolvm/.build/local-live-fork-timing/samples.json) · [VMM 恢复样本](/home/dev/workspace/deepfork-smolvm/.build/local-live-fork-boot-timing/samples.json) · [历史受管实测](performance-wp22.md) · [Boxd 官方 Fork 指南](https://docs.boxd.sh/guides/fork) · [Boxd 云端实测](/home/dev/workspace/deepfork-smolvm/platform-smolvm/docs/smolvm-phase2-polish-2026-11-26/02_backend/reference/experiments/boxd-cli-product-experience-report-2026-11-26.md:83)

### Worker 内核候选

隔离 KVM 用例验证普通目标 UID 被拒绝，授权启动阶段可以导入，随后关闭源 pidfd、永久降权、清空能力并由 seccomp 拦截后续 CoW syscall。子 VM 恢复运行，源退出后仍能首次读冷页并保持各自写入隔离。后续集成候选已接通 resourced 与 managed 启动，并通过受管磁盘、anchor 丢失、同点重试和跨 Worker 内核启动的本机验收。

部署沿用 Boxd 自托管的顺序。校验固定 Kernel/Runtime 制品，给专用 KVM Worker 安装候选并保留旧启动项，在重启后验 KVM、CoW 和真实 Fork，再注册接流量。在 PVE 场景，启动的补丁内核位于运行 Worker 的那台 PVE VM，业务 Guest 继续使用自身内核。已有 Guest 启用新路径需要以匹配的 Runtime 内存配置启动或恢复。当前只有测试包和临时 Worker 引导证据，发行版内核包、签名、GRUB 安装和回退仍待专用 Worker 验收。

[CoW 合入交接](kernel-cow-closeout-2026-11-28.md) · [跨 UID KVM 日志](/home/dev/workspace/deepfork-smolvm/.build/kernel-cow-worker/kvm-worker-serial.log) · [Boxd 自托管安装](https://github.com/boxd-sh/self-host/blob/main/docs/INSTALL.md#install-on-bare-metal)

### Agent Session 接续

Boxd 官方文档规定托管会话随 Fork 复制，子会话处于 `idle`。源机继续当前轮，子机保留会话并等待下一条指令。此前对 Boxd 的真实模型体验停在登录或认证阶段，源轮续跑仍待实测。11 月 28 日的本平台候选已在隔离 KVM 上通过 Native 和 Kernel CoW 的双子 HTTPS/SSE 夹具。父机保留原连接并收到后段，两个子机只读到已继承的前段。上游只收到一次原请求，各分支使用各自的 execution 授权。[父机模型流验收](kernel-cow-closeout-2026-11-28.md)

把平台托管的 Codex / Claude Code 接续列为独立后续项，可先复用现有 Native Live Fork、Guest gate 和运行租约。第一版原型需要在持久化的轮次边界复制已完成对话，子会话从 `idle` 开始。子机放行前隔离继承的旧执行与队列，避免重发模型请求或工具写入。工作区仍来自原 VM 捕获点，可能包含源机本轮已经写入的文件。手动启动的任意 Guest Agent 仍遵循普通 VM Fork，待产品测试验证其各自的重连行为。

**托管会话的产品验收以 Fork 时最后一个完整轮次为准。** 例如前 5 轮已提交，第 6 轮正在收模型回复或等待工具结束，此时创建一个或多个子机。父机用原请求继续并完成第 6 轮。每个子会话只显示前 5 轮，从 `idle` 等待自己的下一条指令。第 6 轮尚未完成的用户消息、半段回复和待执行工具都不进入子机会话历史或执行队列。如果第 6 轮在 Fork 捕获前已经提交，则每个子会话都保留前 6 轮。会话边界不回滚工作区，捕获前已经落盘的文件仍按 VM Fork 语义继承。

分别在提交第 6 轮前后触发 Fork，测试单子机和多子机。独立记录上游请求次数和工具执行次数，核对进行中的父轮只执行一次。随后分别给子会话发送新指令，从各自继承的完整历史继续。父机原模型连接的尾事件、子机初始 `idle` 和子机能独立开始下一轮，分别提供证据。复测断线、请求去重、未知效果和保持运行租约。完整托管会话另立任务，本轮先验父机原模型流续传。

Boxd 的 Fork 指南说明运行中的源机经过短暂停顿后继续。Sessions 指南明确子机继承会话并进入 `idle`，避免重复运行。官方博客说明编码 Agent 把会话保存在机器的持久磁盘上。上述资料没有具体规定半轮消息如何展示，前述“最后一个完整轮次”是本产品明确采用的边界。[Boxd Fork 指南](https://docs.boxd.sh/guides/fork) · [Boxd Sessions 指南](https://docs.boxd.sh/guides/agents/sessions.md) · [Boxd 编码 Agent 博客](https://boxd.sh/blog/coding-agents)

[Boxd Sessions 文档](https://docs.boxd.sh/guides/agents/sessions) · [Agent 实验记录](/home/dev/workspace/deepfork-smolvm/platform-smolvm/docs/smolvm-phase2-polish-2026-11-26/02_backend/reference/experiments/boxd-supplement-2026-11-26/SUPPLEMENT_REPORT.md:51) · [后端范围](/home/dev/workspace/deepfork-smolvm/platform-smolvm/docs/smolvm-phase2-polish-2026-11-26/02_backend/BACKEND_RESEARCH_AND_PLAN.md:93)

## 7. Boxd 外围功能与集成差距

以 Boxd 官方文档索引、应用目录、自托管文档及合作集成指南为外部基线，对照平台代码候选 `polish/integration@4e1ba47`。下表的“已有”表示在该候选中找到真实入口或可复用链路，生产就绪仍以本报告各验收项为准。“预览”表示界面样例，“缺”表示在当前候选未找到完整的受支持交付路径。Boxd 一栏记录其公开产品说明，第三方连接逐项调用、云端 SLA 和商用合同均未实测。核查时公开应用目录显示 133 项，官方索引另列 32 篇 OSS 适配指南。两者的交付形态不同，数字不相加。[官方文档索引](https://docs.boxd.sh/llms.txt) · [公开应用目录](https://boxd.sh/integrations/) · [OSS 适配指南](https://docs.boxd.sh/use-cases/oss-integrations) · [候选界面的预览门控](../../web/src/main.tsx)

### 身份、协作、连接与密钥

| 外围能力 | Boxd 公开能力 | 当前候选和差距 |
|---|---|---|
| 用户登录、自动化身份 | 浏览器登录后 CLI 关联账号。用户、机器和服务账号 API key 有不同用途和范围。自托管有 OIDC 配置，SAML 按商务对接。[CLI 认证](https://docs.boxd.sh/cli/authentication) · [自托管配置](https://github.com/boxd-sh/self-host/blob/main/docs/REFERENCE.md) · [FAQ](https://boxd.sh/faq/) | 已有 Web 会话、范围 token、CLI profile 和 Guest 绑定授权。缺 Web/CLI/Mac 浏览器 OAuth/OIDC 闭环和租户 IdP 管理。服务账号的租户所有权、轮转和撤销也需独立验收。按 §4 处理登录。 |
| 组织、团队和机器协作 | 组织邀请、Owner/Admin/Member、团队与默认值、为同事建机、组织共享或取消共享。共享时清除个人 Agent 登录态，公开服务链接有独立语义。[组织与团队](https://docs.boxd.sh/guides/organizations) · [机器共享](https://docs.boxd.sh/guides/share-a-vm) | 已有账号权限、Guest scope、私有服务访问票据。组织相关界面含预览。缺组织/团队成员和默认值、机器共享时归属及凭据清理、共享撤销和相关审计的完整流程。[组织界面](../../web/src/features/settings/Previews.tsx) · [平台认证](../../internal/auth/auth.go) |
| 第三方连接目录 | 在控制台或 Guest 授权 OAuth/API key，可用个人连接或命名的组织连接。组织共享机器使用组织连接。支持撤销、重新授权和工具调用。目录覆盖 AI、数据分析、通信、客服、设计、开发、文档协作、财务电商、营销、效率、项目管理及销售 CRM。[Connections](https://docs.boxd.sh/guides/integrations/connections) · [目录](https://boxd.sh/integrations/) | 已有执行绑定的宿主凭据代理，Integrations 页为预览。缺连接注册表、按用户/组织/机器隔离、OAuth 同意与刷新、调用与撤销审计、CLI/SDK 工具发现。需独立处理平台登录、第三方连接与 Agent 模型登录各自的凭据。 |
| GitHub、Slack 及协作工具 | 个人 Git/gh、GitHub OAuth 工具和组织安装的 GitHub App 有不同授权范围；个人 Slack 与 Slack Bot 有不同身份。还覆盖 Linear、Notion、Figma、Gmail/Drive/Calendar、Stripe 等。[目录](https://boxd.sh/integrations/) · [Connections](https://docs.boxd.sh/guides/integrations/connections) | 模板可自行运行相应客户端，CLI 已有仓库类引导。缺跨机器授权复用、按仓库范围的安装或个人授权、团队机器人，以及这些连接的状态和移除。 |
| 环境变量、密钥和外部代理 | 支持按组织、团队和机器注入变量或密钥，以及用宿主代理处理指定外部凭据。文档另有外部密钥工具与 broker 适配。[Env/Secrets](https://docs.boxd.sh/guides/env-secrets) · [Egress](https://docs.boxd.sh/guides/egress) · [seekrit](https://docs.boxd.sh/use-cases/oss-integrations/seekrit) · [varlock](https://docs.boxd.sh/use-cases/oss-integrations/varlock) | 已有任务环境保护和 Guest 授权的代理，Environment 页为预览。缺租户级密钥库、作用域覆盖和注入、轮转/撤销状态、Fork 和共享变更后的策略以及外部密钥提供方适配。[任务密钥](../../internal/platformapi/task_secrets.go) · [Guest 授权](guest-delegation.md) |
| MCP、Guest 工具与技能安装 | 集成工具可通过 Guest 的 `run`、强类型 `@boxd/run` 和 MCP 工具供脚本或 Agent 使用。官方提供 CLI 和环境技能，可从本机或 Guest 驱动平台。[集成概览](https://docs.boxd.sh/guides/integrations/overview) · [run CLI](https://docs.boxd.sh/cli/run) · [Skills](https://docs.boxd.sh/reference/skills) | 已有 `sandboxctl mcp` stdio 服务面，提供机器、操作、Fork 和执行工具，以及受界定的 Guest 委派。缺第三方连接的 MCP/CLI 工具面、Agent 技能安装和模板内的受支持配置。[平台 MCP](../../cmd/sandboxctl/mcp.go) · [Guest 授权](guest-delegation.md) |

### Agent、触发器与自动化

| 外围能力 | Boxd 公开能力 | 当前候选和差距 |
|---|---|---|
| 编码 Agent 与会话管理 | 托管 Claude Code、Codex、Antigravity 会话，支持账号或 API key 认证、模型设置、轮次排队/中断、事件流、历史与关闭，定义睡眠和 Fork 语义。开发镜像的预装工具还包括 OpenCode。[Agents API](https://docs.boxd.sh/guides/agents) · [认证](https://docs.boxd.sh/guides/agents/sign-in) · [会话](https://docs.boxd.sh/guides/agents/sessions) · [历史](https://docs.boxd.sh/guides/agents/history) · [Coding Agents](https://docs.boxd.sh/use-cases/coding-agents) | 已有 Guest 执行、租约、凭据代理，Agents 页为预览。缺 Agent 会话 API、模型登录与续期、轮次和事件历史，以及 §6 的接续验收。 |
| 外部托管 Agent 接入 | 官方给出 Anthropic Claude Managed Agents、OpenAI Agents API、Google Gemini Managed Agents 的用例，由外部服务运行 Agent 循环，Boxd 承接工具执行。[Anthropic 方案](https://docs.boxd.sh/use-cases/claude-managed-agents) · [OpenAI 方案](https://docs.boxd.sh/use-cases/openai-agents-api) · [Gemini 方案](https://docs.boxd.sh/use-cases/gemini-managed-agents) | 可用 SDK 和 Guest 执行拼装。缺维护的适配器、事件和工具响应映射，以及外部取消、租约、重连和重放保护的端到端证据。 |
| 自动化脚本、定时与事件 | 强类型 TypeScript 脚本，支持 cron、第三方 webhook/事件、持久 KV、权限范围、连接待就绪、退避重试及历史。目标机器休眠时可唤醒并缓冲事件；Fork 或从 Snapshot 创建时继承的作业默认暂停，隔离模式无继承。[脚本](https://docs.boxd.sh/guides/automations/scripts) · [作业](https://docs.boxd.sh/guides/automations/jobs) · [机器 SDK](https://docs.boxd.sh/guides/automations/managing-machines) | 机器 Automations 页已接持久任务及真实服务，已有 operation、任务恢复和访问唤醒。缺 cron/webhook 调度、事件去重与缓冲、作业历史/KV、连接依赖和父子继承策略。开启前需验相同事件只产生一次外部效果。[现有任务界面](../../web/src/catalog.tsx) · [任务 API](../../internal/platformapi/tasks.go) |
| 到期与任务完成后清理 | 支持绝对时间、相对时间和 cron 到期删除，睡眠时也生效；Guest 可经明确确认请求自身删除。[到期删除](https://docs.boxd.sh/guides/delete-after) · [Guest 自删除](https://docs.boxd.sh/guides/self-delete) | 已有空闲休眠、运行租约和显式所有者删除。缺持久到期删除与 Guest 自删除授权，批量预览/CI 目前需要外部控制器做资源对账和清理。[Guest 委派范围](guest-delegation.md) |

### 客户端、桌面与浏览器

| 外围能力 | Boxd 公开能力 | 当前候选和差距 |
|---|---|---|
| CLI 分发、macOS 菜单与 IDE | macOS、Linux、Windows 安装路径。CLI 可设置 SSH 配置供本地别名及 VS Code、JetBrains 使用，菜单栏连接本地常驻组件。[CLI 安装](https://docs.boxd.sh/cli/installation) · [CLI 功能](https://boxd.sh/features/cli) · [菜单栏](https://docs.boxd.sh/guides/menu-bar) | 已有 Linux 和 Darwin 构建、引导、REPL、菜单源码、托管 SSH 与 SSH 配置能力。缺部署站点上的受支持下载、升级与干净机验证，Mac 仍需签名/公证和真机。Windows、菜单常驻体验与 IDE 联测也待覆盖。详见 §3。[CLI 工作流](cli-workflows.md) · [Mac 构建](../../packaging/macos/README.md) |
| 手机接入与二维码交接 | 手机网页或扫码交接，登录后按次签发限时 SSH 凭据并打开 iOS/Android 的 SSH 客户端。[Mobile](https://docs.boxd.sh/guides/mobile) | 已有 Web 终端和桌面 CLI 每次 SSH 会话授权。二维码组件是预览；缺手机认证回跳、限时凭据和客户端深链的真实流程。[二维码预览](../../web/src/features/console/PreviewQR.tsx) |
| 本机工具桥 | 用户选择启用笔记本客户端。机器能使用指定设备的文本/图像剪贴板、受限的只读本地文件，以及独立 Chrome profile、CDP/Playwright 和本地网络。[Client Utilities](https://docs.boxd.sh/guides/client-utilities) · [浏览器](https://docs.boxd.sh/guides/launch-a-browser) | 已有机器文件访问、终端复制、菜单。缺受信设备注册、逐设备同意和撤销、本机文件/剪贴板桥、Chrome profile 及设备选择。 |
| 文件在本机、Guest 和工具间流转 | CLI/控制台提供文件和目录传输，有授权的 Guest CLI 可直传其他机器。SDK 提供流式大文件及文件系统管理、类型化错误。[File Operations](https://docs.boxd.sh/guides/file-operations) · [Filesystem API](https://docs.boxd.sh/guides/machine-filesystem) | 已有 SSH/SFTP 和授权绑定的列表、下载及有界上传。未找到原生 Guest 到 Guest 的同类命令、同等目录管理 SDK 或对外承诺的通用流式大文件写入。当前使用 64 MiB 上传合同或现有 SSH 工具，并保留不确定写入语义。[CLI 工作流](cli-workflows.md) · [文件 API](../../internal/platformapi/files.go) |
| Guest 桌面、浏览器和 Computer Use | 浏览器可见桌面及人工接管、拖文件。Guest 浏览器支持有头/无头及 Playwright/CDP。Agent 能通过 API/SDK 截图、点击和输入；自动化可选 Guest 或本机浏览器。[桌面](https://docs.boxd.sh/guides/desktop) · [Computer Use](https://docs.boxd.sh/guides/computer-use) · [自动化浏览器](https://docs.boxd.sh/guides/automations/browser) | 已有 System Guest 的授权 VNC/noVNC、票据和活动保护。缺对外支持的截图/坐标输入工具协议、浏览器编排、图像/文件拖放工作流和提供方适配器。[桌面鉴权](../../internal/platformapi/desktop.go) · [Web 桌面](../../web/src/Desktop.tsx) |

### 网络、服务发布与区域

| 外围能力 | Boxd 公开能力 | 当前候选和差距 |
|---|---|---|
| 开箱 HTTPS、多服务和自有域名 | 默认机器 HTTPS URL，可加端口子域。支持单机自定义域名、组织通配域、DNS/TLS 交付。[HTTPS](https://docs.boxd.sh/guides/https) · [Proxies](https://docs.boxd.sh/guides/proxies) · [Custom Domains](https://docs.boxd.sh/guides/custom-domains) | 已有配置型 apps 域、`web_url`、服务发布、真实就绪和鉴权票据。缺自助域名归属校验、证书签发续期、域名冲突和撤销流程。Domains 为预览，现有域名从外网仍需 §2 验收。[服务 API](../../internal/platformapi/services.go) |
| 公网 TCP/UDP 与防爬虫 | 原始公网 TCP/UDP 端口有独立入口。防机器人可按配置在休眠唤醒时或所有访客请求上挑战。[Port Forwarding](https://docs.boxd.sh/guides/port-forwarding) · [Bot Protection](https://docs.boxd.sh/guides/bot-protection) | 已有 HTTP/HTTPS、SSH 和 TCP mTLS 等受控服务路径、访问租约及不重放机制。缺面向用户的原始公网 TCP/UDP 发布和公网防机器人唤醒策略。 |
| 私网、VPN 和出口策略 | 虚拟机互访、Tailscale/NetBird 私有域名和入口，以及域名或地址范围的出站规则。[VM 互访](https://docs.boxd.sh/guides/vm-to-vm) · [Tailscale](https://docs.boxd.sh/guides/tailscale) · [NetBird](https://docs.boxd.sh/guides/netbird) · [Egress Allowlist](https://docs.boxd.sh/guides/egress-allowlist) | 已有运维配置的私网、IP/DNS、附件隔离及部分网络恢复实测，亦有指定宿主凭据代理。缺自助团体网络/VPN 联接及域名、CIDR 出站策略的租户入口。真实跨 Worker 访问仍待双机验证。[网络 API](../../internal/platformapi/networks_linux.go) · [私网实测](private-network-a13.md) |
| 区域与数据驻留 | 公开 Frankfurt/New York，按机选区，同组织使用统一身份和密钥。[Regions](https://docs.boxd.sh/guides/regions) | 独立多节点候选已具备登记、固定归属和放置，本机证据见 §5。缺真实双物理机的覆盖、区域选择/默认区、数据驻留和跨区域凭据边界。 |

### 开发工作流、生态和运维

| 外围能力 | Boxd 公开能力 | 当前候选和差距 |
|---|---|---|
| 开发镜像与可复现环境 | 默认镜像含 Docker Engine、Compose/Buildx 和常用 Agent；Machine Spec 定义来源、资源、网络、变量、连接与首次启动命令，并支持版本和更新。[Docker](https://docs.boxd.sh/guides/run-docker) · [Machine Specs](https://docs.boxd.sh/guides/machine-specs) · [开发工作区](https://docs.boxd.sh/use-cases/development-workspaces) | 已有模板、配置调整、注册/准入与 Docker Compose/K3s 实测。缺面向用户的配套开发镜像、模板升级路径，以及完整跨资源 Spec 事务和冲突预演。后者继续按原计划后置。[Guest 运行时实测](guest-runtime-a17.md) |
| PR 环境与 CI | GitHub webhook/Actions、按 PR 建机并发布稳定 URL，更新评论及关闭清理。可从最近成功的主干保存环境并行构建，回传状态和制品；另有自托管 Actions Runner 方案。[PR Preview](https://docs.boxd.sh/use-cases/preview-environments) · [CI Runners](https://docs.boxd.sh/use-cases/ci-runners) · [自托管 Runner](https://boxd.sh/blog/self-hosted-actions-runner/) · [保存应用环境](https://docs.boxd.sh/guides/snapshot-your-app) | 已有 SDK/CLI、模板、保存点、Fork、服务 URL 和任务。缺交付可安装的 GitHub App/Action 或 Runner 指南、PR/commit 幂等映射、状态和制品回传、关闭后自动清理。 |
| 合作生态与接口 | Python/TypeScript SDK、自助 gRPC/proto，以及编码代理、会话工作区、评测、SaaS、RL、编排、个人助手等用例。OSS 目录单列外部工具的适配指南。[gRPC](https://docs.boxd.sh/reference/grpc-api) · [Proto](https://docs.boxd.sh/reference/grpc-proto) · [OSS 目录](https://docs.boxd.sh/use-cases/oss-integrations) · [Agentic SaaS](https://docs.boxd.sh/use-cases/agentic-saas) · [RL](https://docs.boxd.sh/use-cases/reproducible-rl-environments) · [个人助手](https://docs.boxd.sh/use-cases/personal-assistants) | 有 HTTP API、Python 与 TypeScript SDK 和平台 MCP。缺公开 gRPC 服务及已维护的合作适配。协议和框架支持应按真实调用链确认，所列用例可先用现有 SDK 形成参考实现。 |
| 灾备与机器存储交付 | 支持按机定时或按需备份磁盘到对象存储、原地恢复，恢复范围为磁盘。[Disaster Recovery](https://docs.boxd.sh/guides/disaster-recovery) | 有备份策略、留存、闭包校验和独立 Worker 恢复证据。现有配置以运维管理的存储目录为主。缺租户自助的按机磁盘灾备、对象存储凭据/保留管理和恢复界面。[备份合同](BACKUPS.md) |
| 自托管、支持与访问审计 | 文档覆盖 GCP/GKE、AWS 和既有 KVM 服务器，公布安装/校验流程；网站提供 BYOC、企业支持和状态页。第三方连接后端可在自托管方案中另配。[Self-hosting](https://boxd.sh/self-hosting/) · [安装文档](https://github.com/boxd-sh/self-host/blob/main/docs/INSTALL.md) · [状态页](https://status-boxd.com) | 已有 KVM/PVE 安装、回退、真实 metrics、事件及审计记录。缺经过验证的云端安装模板、外部状态/事故通知和租户级审计交付。企业 SLA、合规与支持按实际合同另行确认。[安装说明](../../packaging/install/README.md) · [验收与运维](ACCEPTANCE_AND_OPERATIONS.md) |
| 用量、计费和付款 | 公布 CPU/RAM/磁盘计费、生命周期价格、额度、充值与企业方案。[资源限制](https://docs.boxd.sh/guides/resources) · [Pricing](https://boxd.sh/pricing/) · [FAQ](https://boxd.sh/faq/) | 有指标、资源预算、准入和配额。缺租户级可对账用量、费率/额度、自动充值、付款和账单。商业化保持后置。 |

OSS 核对清单含 TanStack AI、Flue、Bindu、Harbor、Crabbox、Zuse、Omnara、Convex、herdr、Scale AI AgentEnv、Paperclip、Pydantic AI、rakazo、Agent Swarm、Sheprd、Tardigrade、Bast、AG2、seekrit、varlock、LangChain、Dify、Nod、Pi、Inspect AI、Sixb、Bespoke Labs Sandbox、Agent Foundation (a13n)、Argon、AgentRun、Mirage、DeerFlow。适配指南的存在只证明有公开集成路径，各项目版本兼容和实际运行结果需按选定样本核实。[OSS 目录](https://docs.boxd.sh/use-cases/oss-integrations)

明天先验已有的下载与登录、SSH/IDE、私有 HTTPS、已发布服务就绪、SDK/CLI 文件和操作、备份恢复。按 §3–§5 单列需要真机、生产权限和独立 Worker 的验证。最贴近现有底座的后续集成是浏览器登录、可安装的客户端，以及一条 GitHub PR 预览链路。后者可先用现有 API/SDK 串起创建、就绪、评论和关闭清理，再评估 GitHub App、凭据代理、cron/webhook 与 Agent 会话。全应用目录、完整 Machine Spec、客户端设备桥、公网和多区域产品化及商业功能继续独立排期。外围对照不改变 J01–J11 与生产候选范围。

## 8. 明天的外围与双 SDK 验收

官方博客、X 线索和官方 GitHub 的新增发现已转成[外围差距、SDK 方法与验收表](boxd-peripheral-handoff-2026-11-27.md)。其中列出六个有公开源码的官方适配器、Mac 菜单与手机 SSH、默认 URL 自动选端口、休眠作业、连接权限和 GitHub PR/CI 的交付边界。

- [ ] **先验当前制品。** 从同一候选打包 Python wheel 和 npm tarball，在干净环境安装，两套 SDK 各走一次建机、等原 operation、确认 Guest 可执行、文件校哈希、观察服务就绪和清理。若使用低层 API 或 CLI，准确记录缺失的具名 SDK 方法。超时通过原 operation 对账。
- [ ] **先补已有 API 的 SDK 封装。** 盘点原生 Exec/PTY、机器改名/删除、Service 修改/撤销，以及 Guest 与 Service 就绪等待。两语言合同明确退出码、取消、状态与效果，再评估 Python 异步、事件重连封装、浏览器与边缘运行时。
- [ ] **登记两个独立样板。** GitHub PR 预览从同一 PR 更新和关闭清理起步。定时任务从 Hibernate 后按时唤醒、相同运行槽位只执行一次起步。两者分别记录外部测试仓库、权限和隔离 Worker 等依赖与完成条件。

## 当日结束留档

- 生产候选 Git 提交、版本、制品及校验结果，环境和回退基线。
- J01–J11 证据编号；Mac 下载、签名、公证、真机验收和 UI 截图。
- 自有素材清单、桌面与移动视觉矩阵、首次使用录屏，并登记真实数据与参考样本所用版本。
- OAuth/OIDC 的 provider 与完成边界，双 Worker 拓扑与 N01–N18 实机证据，高并发各档结果。
- Fork 各阶段计时和负载说明，实验 CoW 的 Bundle 与授权边界，以及托管 Agent 接续的模型或测试替身证据。
- Boxd 外围功能按已有、预览、缺口和待实机验证登记；既有入口附验收结果，扩展项附当前缺失环节。
- 双 SDK 干净安装记录、具名方法覆盖、PR 预览及定时任务的负责人、首个样板和验证证据。
- 每个未通过项的重现条件、负责人、是否阻塞生产和验证后需要收尾的分支。
