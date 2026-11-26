# 2026-11-27 生产验收与发布收尾

交接时间为 2026-11-26。明天先锁定一个生产候选，在生产同配置的隔离环境完整跑通，再在授权窗口使用专用账号和临时资源从外网走一次生产验收。多节点故障演练和大并发压测使用独立环境。以下是待执行计划和已确认的本机证据，今天未部署生产。

## 已确认的代码与分支

| 交付线 | 已验证版本 | 明天的收尾 |
|---|---|---|
| 平台、CLI 和控制台 | `polish/integration@4e1ba470ed66`，schema 1019。此提交通过 Go 全量、宿主原生 OpenSSH、Vite、Darwin arm64/amd64 CLI 构建。最后一轮真实 SSH/HTTPS KVM 验收绑定 `0f35c613d14d`。更早的 J01–J10 与 J11 各有记录，以其各自 SHA 为准。 | 以当前集成分支为起点，生成一套包含平台、CLI 和控制台的冻结制品。用最终候选补同版 J01–J11 核心链路。确认发布目标后合并，保留源提交、版本、构建和回退凭据。 |
| Runtime / Guest | `polish/runtime-candidate@cb31d6c`，测量 Bundle `sha256:3de1c4f1a848edd37b8bc5c94b944e0dd5c18ab3d90874a1348ecc0529710a3c`。同期还留有 `polish/native-files@2b172e8`、`polish/runtime-glue@b724f99`。 | 按发布源和最终候选比较切片差异，核对实测文件权限、Agent 版本、Runtime 及 reader。确认成果和证据已被候选包含后再收源分支和工作树。 |
| 多节点 | `feat/multi-node-v1@1c039e3791f1`，实际发布包源码 `53fefd0`，schema 1025。同一台主机真实跑过一个平台和两个回环 mTLS Node，N01–N18 的 41 个具名测试通过。 | 从两条稳定线建临时合并候选，解决 API、CLI、网关、机器视图和生成产物冲突后重建。逐项核对仍有历史提交的 `feat/multi-node-gateway`、`feat/multi-node-owner`、`feat/multi-node-placement`，按实际差异决定是否回收。先在独立部署验证 1019→1025、兼容性和回退，再考虑进入共同发布候选。 |
| 内核 | 独立仓库 `feat/kernel-cow-v1@8d181b563459`。双 Guest 冷读及写隔离两次 AMD 嵌套 KVM 通过。 | 保留独立交付与证据。需要纳入某个 Runtime 时另起候选并用对应内核、libkrun 和 Runtime 完整验收。 |
| 已回收开发分支 | 已回收 31 个工作树，删除 10 个已合并本地分支，`archive/polish-closeout/` 保留 7 个开发切片或诊断引用。 | 按记录核对。已集成的切片无需重合；`polish/backend`、`polish/frontend` 可在发布提交与交接记录对齐后按需收尾。 |

[平台验收记录](/home/dev/workspace/deepfork-smolvm/platform-smolvm/.git/polish-coordination/integration-status.md) · [SSH / HTTPS 实测清单](/home/dev/workspace/deepfork-smolvm/.build/polish-20261126/final-0f35c613d14d/managed-access-verification.json) · [分支归档清单](/home/dev/workspace/deepfork-smolvm/.build/polish-20261126/git-closeout/branches.tsv) · [多节点实现和验收](../../../multi-node/docs/deep-runtime/multi-node-v1.md) · [内核验证](/home/dev/workspace/deepfork-smolvm/kernel-cow-k1/linux-patches/evidence/anchor-exit-2026-11-26/README.md)

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

## 当日结束留档

- 生产候选 Git 提交、版本、制品及校验结果，环境和回退基线。
- J01–J11 证据编号；Mac 下载、签名、公证、真机验收和 UI 截图。
- OAuth/OIDC 的 provider 与完成边界，双 Worker 拓扑与 N01–N18 实机证据，高并发各档结果。
- 每个未通过项的重现条件、负责人、是否阻塞生产和验证后需要收尾的分支。
