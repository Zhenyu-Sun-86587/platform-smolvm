# 生产候选验收与发布收尾

平台源码起点是包含 Kernel CoW 与多 Worker 的 `polish/integration@69a42f4`，schema 1025。Runtime 源码起点是 `polish/runtime-candidate@eace7d59`，Worker schema 32，配套 libkrun 为 `6aef96de`。已验证同机双 Node、独立隔离 Worker 内核、父机模型流和公开接口回归。各实验沿用其原始制品标识。[最新源码与目录](checkout.md) · [多节点证据](multi-node-v1.md) · [CoW 证据](kernel-cow-closeout-2026-11-28.md)

本清单用于后续候选验证。先在同配置的隔离环境跑通，再在授权窗口用专用账号与可回收资源执行生产端到端。双物理 Worker、规模化压力、部署站点下载、浏览器 OAuth/OIDC 和 macOS 真机签名包均列入待验。

## 1. 冻结候选和生产验证条件

- [ ] 记录实际环境的版本、schema、control epoch、Bundle/reader、模板摘要、Worker/KVM、磁盘和有效内存预算。核对控制台 origin、独立的 apps 域名、通配 DNS/TLS 及外部 HTTPS。锁定候选 SHA、制品 SHA-256、签名和下载 URL，保留对应的旧包。
- [ ] 备份 SQLite、配置和可恢复材料，检查备份，并在隔离副本恢复一次。确认旧版本能否读候选 schema、维护状态和回退触发条件。涉及 schema 回退时使用相容备份与匹配二进制，验证新的 control epoch 能挡住旧请求。
- [ ] 准备专用测试部署或获准的测试资源范围、测试账号、不同范围的凭据、资源清单、可观察的后端服务、外网客户端和变更窗口。升级、断电和压测只作用于获准的测试机器与节点。保留用户工作负载及 PVE 管理网络。有阻塞先标出缺少的主机、权限、证书或时间窗口。复用开发机时，构建按 `heavy.lock` 排队，KVM 和压力实验先拿该锁，再拿 `worker-lab.lock`。

[安装、备份与回退](../../packaging/install/README.md) · [Worker 预检](checkout.md#首次安装前的只读隔离预检)

## 2. 一条生产端到端工作流

| 顺序 | 必跑检查 | 验收证据 |
|---|---|---|
| 登录、下载、建机 | 外网登录 Web，签发最小范围的 CLI token，从部署后的下载入口获取已签名或有可信校验清单的客户端。使用新配置目录登录、建机、查询、改名。另测 read/tunnel 凭据、过期和撤销。 | 构建版本、下载摘要、operation ID、授权结果；生产请求使用真实 API。 |
| 连接、文件和服务 | 终端、PTY resize、远程退出码、无感 SSH 和旧密钥；SFTP 和平台 API 传输分别校验字节。测试 64 MiB 上传边界、取消、回执丢失和执行换代。发布私有 HTTPS，测试登录跳转、凭据与浏览器票据、真实就绪和慢请求期间的活动保护。 | 文件 SHA-256、请求 ID、绑定和 execution epoch、授权及服务观察；不确定写入只查原意图，HTTP POST 只执行一次。 |
| Fork 和保存点 | 源运行时做独立 Live Fork，核对内存、根盘和数据卷；做批量部分成功、对失败子项重试。创建 VM 保存点，修改源配置，再验证同机回滚、从点建机、卷冲突策略和旧点可读。 | 父子读写隔离、capture ID、原 operation/child/attempt、捕获配置与恢复观察。 |
| 生命周期和清理 | Standby、Hibernate、Wake、冷 Stop/Start；覆盖默认策略、实例覆盖、运行租约及模型等待。被动状态、metrics、inventory 不触发唤醒；核对指标范围、采样时间、不可用字段和执行身份。删除本次创建的机器、卷、服务、点和凭据。 | 权限/唤醒事件、Lease 与真实 Worker 观察；清理前后资源、quota 和 reservation 对账。 |

主流程通过后再在隔离部署演练重启、升级和恢复。验收记录包含版本与制品哈希、步骤、日志、错误码、重试及可定位的失败原因。每项标记通过、失败或未验证。仅 UI 可见或 API 受理不足以证明 Guest 或服务已就绪。

[现行回归入口](ACCEPTANCE_AND_OPERATIONS.md#常用回归) · [CLI 命令与示例](cli-workflows.md)

## 3. macOS、CLI、外观与发布入口

| 工作 | 当前状态 | 发布验收 |
|---|---|---|
| 从部署站点下载 CLI / Mac | 现有 Linux 服务端安装包包含 `sandboxctl`，已生成 Darwin CLI 构建。源码中未找到面向用户的客户端发布下载入口。 | 在管理站点提供清楚的 Download 入口，按候选支持范围链接 Linux CLI、macOS arm64/amd64 CLI 和 macOS App。每个资产提供不可变版本、可信校验清单或签名、最短安装说明。用无预装、无缓存的外网客户端验证下载、版本、首次安装、升级、旧版本链接及失败提示。 |
| macOS App 和 CLI | 已有引导、REPL、动画和菜单栏源码；Darwin 双架构 CLI 已编译通过。AppKit 真机操作、签名与公证仍待验证。 | 用 Apple Silicon 和 Intel 真机或明确记录实际覆盖，在干净账号验证首次启动、退出重开、切换 profile、创建、终端与 SSH 别名、Web URL、睡眠与唤醒。CLI 覆盖 PATH、zsh、非 TTY、`--json`、脚本退出码、终端缩放、跳过动画、减少动态。用真实发布产物验证 helper、Developer ID 签名、`notarytool` 公证、staple 和 Gatekeeper。 |
| 外观替换 | 原型通过 `SANDBOXCTL_BRAND_DIR` 读取可替换的 terminal 字样和菜单图标。 | 验收自有品牌的 CLI 字样与动画、终端静态版、浅色/深色菜单图标、App 图标和控制台登录/下载页。截取正常、窄终端和减少动态三组证据，核对公开包的素材来源和授权。 |

Mac 真机、Developer ID 与公证凭据未就绪时，可以验证内部包，并把公开下载发布标记为待验证。CLI 与 App 是否必须同批对外发布，以签名和干净设备验收结果决定。

外观调整等实际体验反馈后再安排。发布验证先覆盖下载、登录、首台机器、首次连接和当前包的素材授权。

[macOS 构建和品牌入口](../../packaging/macos/README.md) · [首次启动样例](/home/dev/workspace/deepfork-smolvm/.build/polish-20261126/cli-onboarding-macos/final-5c153be/prototype/README.md) · [Apple 公证文档](https://developer.apple.com/documentation/security/notarizing-macos-software-before-distribution)

## 4. 浏览器 OAuth / OIDC 登录

当前 Web 用账号密码和会话。CLI 交互引导可以打开 API Keys 页面，用户仍需粘贴平台 token，菜单栏复用该 CLI profile。当前候选未发现已接入的人类用户 OAuth/OIDC 登录链路。本轮将现有 token 下载登录路线先验收，再用独立任务验证 OAuth/OIDC。

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

[多节点 N01–N18](multi-node-v1.md#n01n18-本地证据) · [多节点登记与换证](WORKER-ENROLLMENT.md) · [Worker 验收入口](ACCEPTANCE_AND_OPERATIONS.md#常用回归)

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

在固定候选、同一源负载下，同时记录请求受理、源 vCPU 停顿、子 vCPU 恢复、首次 Guest `exec`、首次 SSH 和 HTTPS 业务响应。用子机身份与继承值核对 HTTP 响应，记录是否走热连接。优先复测受管 Native 路径，单独量化 Worker 轮询、盘切层和 Guest 身份激活。用当前候选实测启动派发等待和事件唤醒成本，再设定相同口径的目标。小样本列实际范围，规模上来后再报告分位。

[本地 CLI 样本](/home/dev/workspace/deepfork-smolvm/.build/local-live-fork-timing/samples.json) · [VMM 恢复样本](/home/dev/workspace/deepfork-smolvm/.build/local-live-fork-boot-timing/samples.json)

### Worker 内核候选

隔离 KVM 用例验证普通目标 UID 被拒绝，授权启动阶段可以导入，随后关闭源 pidfd、永久降权、清空能力并由 seccomp 拦截后续 CoW syscall。子 VM 恢复运行，源退出后仍能首次读冷页并保持各自写入隔离。后续集成候选已接通 resourced 与 managed 启动，并通过受管磁盘、anchor 丢失、同点重试和跨 Worker 内核启动的本机验收。

部署沿用 Boxd 自托管的顺序。校验固定 Kernel/Runtime 制品，给专用 KVM Worker 安装候选并保留旧启动项，在重启后验 KVM、CoW 和真实 Fork，再注册接流量。在 PVE 场景，启动的补丁内核位于运行 Worker 的那台 PVE VM，业务 Guest 继续使用自身内核。已有 Guest 启用新路径需要以匹配的 Runtime 内存配置启动或恢复。目前留有 canary 包和隔离 Worker 启动证据，发行版内核包、签名、GRUB 安装和回退仍待专用 Worker 验收。

[CoW 合入交接](kernel-cow-closeout-2026-11-28.md) · [跨 UID KVM 日志](/home/dev/workspace/deepfork-smolvm/.build/kernel-cow-worker/kvm-worker-serial.log) · [Boxd 自托管安装](https://github.com/boxd-sh/self-host/blob/main/docs/INSTALL.md#install-on-bare-metal)

### Agent Session 接续

Boxd 官方文档规定托管会话随 Fork 复制，子会话处于 `idle`。源机继续当前轮，子机保留会话并等待下一条指令。此前对 Boxd 的真实模型体验停在登录或认证阶段，源轮续跑仍待实测。11 月 28 日的本平台候选已在隔离 KVM 上通过 Native 和 Kernel CoW 的双子 HTTPS/SSE 夹具。父机保留原连接并收到后段，两个子机只读到已继承的前段。上游只收到一次原请求，各分支使用各自的 execution 授权。[父机模型流验收](kernel-cow-closeout-2026-11-28.md)

把平台托管的 Codex / Claude Code 接续列为独立后续项，可先复用现有 Native Live Fork、Guest gate 和运行租约。第一版原型需要在持久化的轮次边界复制已完成对话，子会话从 `idle` 开始。子机放行前隔离继承的旧执行与队列，避免重发模型请求或工具写入。工作区仍来自原 VM 捕获点，可能包含源机本轮已经写入的文件。手动启动的任意 Guest Agent 仍遵循普通 VM Fork，待产品测试验证其各自的重连行为。

**托管会话的产品验收以 Fork 时最后一个完整轮次为准。** 例如前 5 轮已提交，第 6 轮正在收模型回复或等待工具结束，此时创建一个或多个子机。父机用原请求继续并完成第 6 轮。每个子会话只显示前 5 轮，从 `idle` 等待自己的下一条指令。第 6 轮尚未完成的用户消息、半段回复和待执行工具都不进入子机会话历史或执行队列。如果第 6 轮在 Fork 捕获前已经提交，则每个子会话都保留前 6 轮。会话边界不回滚工作区，捕获前已经落盘的文件仍按 VM Fork 语义继承。

分别在提交第 6 轮前后触发 Fork，测试单子机和多子机。独立记录上游请求次数和工具执行次数，核对进行中的父轮只执行一次。随后分别给子会话发送新指令，从各自继承的完整历史继续。父机原模型连接的尾事件、子机初始 `idle` 和子机能独立开始下一轮，分别提供证据。复测断线、请求去重、未知效果和保持运行租约。完整托管会话另立任务，本轮先验父机原模型流续传。

Boxd 的 Fork 指南说明运行中的源机经过短暂停顿后继续。Sessions 指南明确子机继承会话并进入 `idle`，避免重复运行。官方博客说明编码 Agent 把会话保存在机器的持久磁盘上。上述资料没有具体规定半轮消息如何展示，前述“最后一个完整轮次”是本产品明确采用的边界。[Boxd Fork 指南](https://docs.boxd.sh/guides/fork) · [Boxd Sessions 指南](https://docs.boxd.sh/guides/agents/sessions.md) · [Boxd 编码 Agent 博客](https://boxd.sh/blog/coding-agents)

[Guest 委派与模型流验证](guest-delegation.md#active-model-streams-during-fork)

## 7. 客户端和 SDK 发布验收

从同一候选构建 Python wheel 和 npm tarball，在干净环境安装。两套 SDK 各走一次建机、等待原 operation、确认 Guest 可执行、文件校验、服务观察和清理。记录实际使用的方法及低层接口补口。CLI 与 macOS 下载按第三节验证。后续产品能力、集成和具名方法的开发范围以工作区顶层 `docs/phase3/` 为准。

每个验收项记录候选源提交及制品摘要、实际环境、操作与结果、证据位置和剩余阻塞。记录生产回退版本，分别列出实测通过、失败和待验范围。
