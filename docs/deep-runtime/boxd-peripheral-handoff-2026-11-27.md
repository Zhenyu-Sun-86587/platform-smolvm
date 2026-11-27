# Boxd 外围功能深查与下一步验收

对应[生产交接 §7](production-handoff-2026-11-27.md#7-boxd-外围功能与集成差距)的 25 项总表。这一页把官网案例、官方 GitHub 和 SDK 的发现落成可验收的工作。对照代码为 `polish/integration@4e1ba47`，交接文档基线为 `d803d25`。已对照公开参考及源码，本轮未重新运行 Boxd 云账号、外部适配器或本平台生产流程。

## 证据与范围

- 官方博客归档可见 27 篇，重点核对了休眠自动化、首次打开服务、凭据代理、PR 预览、预热 CI、Agent SaaS、客户评测、私有网络和自托管。博客的演示、用例建议和客户成绩记为产品线索。行为和限制相应以指南、公开 SDK 或适配器维护说明为准。[官方博客](https://boxd.sh/blog/) · [文档索引](https://docs.boxd.sh/llms.txt) · [应用连接目录](https://boxd.sh/integrations/)
- 官网链接到官方 X 账号 `@boxd_sh`。X 原页本次返回 403。公开帖文索引提供了 SDK 开源、手机 SSH、NetBird、CPU 按实际用量计费和按 Agent 分配机器的推广线索。只有能回到官方文档或 GitHub 的功能进入下面的差距表。主题帖未给出可复现的链路计时或容量条件，相关宣传数字不作为我们的发布门槛。[官方 X](https://x.com/boxd_sh) · [公开帖文索引](https://lurkbird.com/boxd_sh) · [官方移动文档](https://docs.boxd.sh/guides/mobile) · [NetBird 指南](https://docs.boxd.sh/guides/netbird) · [计费说明](https://boxd.sh/pricing/)
- 官方 GitHub 组织公开列出 SDK 镜像、自托管安装器和六个独立适配器。SDK 源码核对基线 `86c8955bcaf9`。核查时 npm 和 PyPI 均发布 Boxd SDK `0.3.6`。本平台仓库声明 `@sandbox-platform/client` 与 `sandbox-platform` 都是 `0.1.0`，查询对应公开 registry 的精确版本路径均返回 404。内部制品或私有 registry 未核实。[GitHub 组织](https://github.com/boxd-sh) · [SDK 源码基线](https://github.com/boxd-sh/sdk/tree/86c8955bcaf9a7410a8ee364bb572e9a07ab47cf) · [npm 版本记录](https://registry.npmjs.org/@boxd-sh%2fsdk/0.3.6) · [PyPI](https://pypi.org/project/boxd/0.3.6/) · [本平台包配置](../../sdk/typescript/package.json) · [Python 包配置](../../sdk/python/pyproject.toml)

## 1. SDK 产品化和现有 API 覆盖

两套 SDK 已有分页、operation 轮询、Fork 批量和子项重试、保存点、卷、租约、指标、文件和桌面入口。优先让应用开发者在两种语言中完成同一条真实工作流，再决定是否扩展语言和传输。平台 API 的 OpenAPI、真实能力和 `Effect` 语义是本地客户端合同。[平台 OpenAPI](../../api/openapi.yaml) · [TypeScript 客户端](../../sdk/typescript/src/index.ts) · [Python 客户端](../../sdk/python/sandbox_platform/__init__.py)

| 核查项 | Boxd 证据 | 我们已有的入口和待办 |
|---|---|---|
| 安装与生成 | PyPI、npm、版本标识，Python 和 TS 同源 proto，分别提供入门和完整方法文档。[SDK 仓库](https://github.com/boxd-sh/sdk) | 有项目源码、Python 类型标记、TS 包和声明文件。明天从同一候选生成 wheel 和 npm tarball，在干净目录安装与导入，核对版本、校验及平台兼容范围。公共 registry、版本兼容说明及 API/SDK 覆盖检查单列。 |
| 机器就绪与服务就绪 | `wait_until_ready` / `waitUntilReady` 等到机器可执行。create 和 fork 先返回。[Python 方法](https://docs.boxd.sh/reference/python-sdk#machines) · [TS 方法](https://docs.boxd.sh/reference/typescript-sdk#machines) | 已可等 operation，Machine 有能力/执行信息，Service 有观察。缺具名的 Guest 与 Service 等待方法。SDK 应用验收分别记录 operation 终态、目标 execution 可用和业务探针响应。被动等待不唤醒休眠 Guest。 |
| 日常管理方法 | Python 和 TS 命名空间覆盖机器名/ID、删除、更新和代理管理。[Python 方法](https://docs.boxd.sh/reference/python-sdk#machines) · [TS 方法](https://docs.boxd.sh/reference/typescript-sdk#machines) | 平台已有 rename/删除、Service 修改/撤销 API，有 `mutate` 通用入口，两个 SDK 缺对应的具名封装。优先加薄封装和两语言合同测试，沿用 revision、generation 和 intent。有歧义的名称按已有 CLI 规则报错，程序优先持有 ID。[机器路由](../../internal/platformapi/settings.go) · [Service 编辑路由](../../internal/platformapi/service_edit.go) · [Service API](../../internal/platformapi/services.go) |
| Exec、PTY 与流 | Boxd 有同步命令、二进制输出流和交互式流，TS `/web` 有浏览器请求流限制。[Python SDK](https://docs.boxd.sh/reference/python-sdk) · [TS Web 入口](https://docs.boxd.sh/reference/typescript-sdk#cloudflare-workers-browsers-and-other-non-node-runtimes) | 本地有 Task 输出游标、低层 WebSocket/ticket、CLI 原生 Exec/PTY 和 Python **Guest** `exec`。通用 Python/TS 两个 Client 缺覆盖主机端原生 Exec/PTY 的具名入口。验二进制 stdout/stderr、远程退出码、stdin/resize、断线和取消。以公开协议区分停止观察、分离 PTY 与取消执行。[OpenAPI](../../api/openapi.yaml) · [Guest Client](../../sdk/python/sandbox_platform/guest.py) |
| 文件与流式传输 | `machines.fs` 有大文件流、目录/属性/重命名和细分文件错误。[Filesystem API](https://docs.boxd.sh/guides/machine-filesystem) | 本平台两 SDK 有文件列表、下载和 64 MiB 有界上传。Python 下载为字节迭代，TS 下载可取响应体。先交付哈希、取消、超限和未知效果样例。通用目录管理、大文件上传合同和 Guest 到 Guest 原生命令后置。 |
| 认证、错误及重连 | Boxd 有 API key 换短会话、凭据解析顺序、细分错误、连接失败重试策略和 Python 异步客户端。[Python SDK](https://docs.boxd.sh/reference/python-sdk#authentication) · [TS 错误](https://docs.boxd.sh/reference/typescript-sdk#errors) | 两客户端已保留 `code`、`effect`、operation ID 等错误合同。Python 通用客户端同步、显式传入 origin/token，TS 有 AbortSignal 和同源 cookie/CSRF。需要统一超时/连接错误示例、SSE 按游标恢复和游标过期重同步、Python 异步客户端需求，以及浏览器/Node/Bun/Deno/Workers 支持矩阵。写超时只查原 operation，无法确认效果时禁止自动重放。 |
| Guest 凭据和生态方法 | Boxd 云内 SDK 可从机器环境自动认证，且保留显式配置优先级。公开 Agent、代理、网络、组织、区域和用量接口。[Guest 认证](https://docs.boxd.sh/reference/python-sdk#inside-a-machine) · [公开 proto](https://github.com/boxd-sh/sdk/blob/86c8955bcaf9a7410a8ee364bb572e9a07ab47cf/proto/api.proto) | Python 有主动发放的 execution-bound Guest Grant 和 vsock，未查到等价的 TypeScript Guest transport 或通用 Client 自动认证。先保留显式授权。Agent、域名/VPN、组织、计费及区域的 SDK 封装跟真实 API 排期。现有 HTTP/JSON 和事件协议先覆盖适配器，不要求新增 gRPC 才开工。[Guest 授权与代理](guest-delegation.md) |

**明天两语言验证。** 对同一发布候选，用 Python 和 TS 从打包的制品导入。不依赖 Web 私有模块。记录最小权限 token、版本和 API contract，顺序建机、等原 operation、用已支持入口确认 Guest 能执行、上传和下载校哈希、观察服务就绪、最后清理。覆盖一次等待超时后用同一 operation ID 恢复、一次文件取消和一次冲突/未知效果夹具。某一步需要临时使用低层 API、CLI 或独立 SSH 时，在验收记录中注明缺哪个 SDK 方法，分别记录 API 链路与 SDK 体验结果。

## 2. 定时、事件与第三方连接

Boxd 的博客和作业指南把触发源留在平台，使 Guest 睡眠时仍能收到事件。脚本通过 cron 或间隔声明下一次运行，通过 provider 订阅收到事件。晚醒时补一次错过的 cron slot，唤醒期间到达的事件最长缓冲 15 分钟。以脚本绝对路径识别和替换作业，支持运行日志、暂停、重启、移除、连接等待、有限退避和持久 JSON 状态。一次性脚本失败和后台崩溃重试是两种处理方式。Fork 或从 Snapshot 建出的副本默认暂停继承作业。[睡眠自动化博客](https://boxd.sh/blog/automations-that-sleep/) · [Scripts](https://docs.boxd.sh/guides/automations/scripts) · [Jobs](https://docs.boxd.sh/guides/automations/jobs) · [run CLI](https://docs.boxd.sh/cli/run)

| 可独立交付 | 当前与缺口 | 最小验收 |
|---|---|---|
| 定时作业 | 已有持久任务、活动保护和访问唤醒。缺用户可控 schedule、时区、next run、错过窗口、并发策略和历史。一开始只要求固定 cron/时区和一个确定的补跑规则。 | 用隔离测试 Guest 进入 Hibernate，定时唤醒后执行一次，重启平台后仍能对账。相同 slot 去重，重叠和过期按配置，空闲再次休眠，父子的定时定义遵守暂停/启用策略。 |
| Webhook 与运行记录 | 缺 provider 入口和签名验证、持久 delivery ID、缓冲过期可见性、执行对账及可操作状态。Tasks 界面可以承载运行视图，后台已有 operation 与 task ID。 | 用同一个合法测试事件连续投递，经唤醒产生一次运行和一次可核对外部效果。验无效签名/租户跨界、新旧事件、过期、长请求租约、取消和进程重启。下游结果未知时先对账并标记，禁止无限重试。 |
| 个人和组织凭据 | 已有执行绑定 Guest Grant 和宿主凭据代理。缺 OAuth 连接对象、安装/授权范围、刷新撤销和机器共享过滤。Git 登录、GitHub 用户 API 和 GitHub App 安装分别建模。Slack 个人账号与 Bot 同理。[连接文档](https://docs.boxd.sh/guides/integrations/connections) · [宿主凭据案例](https://boxd.sh/blog/a-secret-the-machine-never-has/) | 先用测试账号交付一条 GitHub App 路径。核验仓库权限范围、撤销即时生效、短期安装凭据和审计、Fork/共享访问隔离。明确平台宿主代理支持的 origin/route 和 secret 生命周期。通用出口占位符产品后置。 |
| 面向用户的自动化界面 | 当前 Automations 有持久任务链路和预览文案。Boxd 由 Guest 文件驱动，控制台显示最近运行、告警、等待连接、流程图和生成说明。 | 先用真实作业和运行记录提供列表、暂停/手工重跑权限、错误原因、日志、最近/下次执行。自动生成说明另外决定代码上传和隐私边界，再补可视流程。 |

**第一条集成样板。** 使用测试仓库的 GitHub App，把 `pull_request` 的打开、更新和关闭绑定到同一 PR 的预览资源。以安装、仓库与 PR 标识确定资源归属，以 delivery ID 去重，等真实 HTTPS 再发预览结果，关 PR 后清理机器、卷、服务和凭据。先用 SDK 复用现有 API 验一条竖切。凭据 UI、通用 Webhook 平台及完整作业引擎各自立项。第二条样板可用单次 GitHub Actions Runner，快照只存工具链与缓存，每个 job 使用独立、短期注册，完工后删除。定时任务验收独立于 GitHub 外部可用性。[PR 预览博客](https://boxd.sh/blog/preview-environments/) · [自托管 Actions Runner 博客](https://boxd.sh/blog/self-hosted-actions-runner/) · [现有 Tasks API](../../internal/platformapi/tasks.go)

## 3. 客户端、入口和开发者体验

| 路径 | Boxd 公开证据 | 本平台待验或待做 |
|---|---|---|
| CLI 安装到首次连接 | 官网提供下载脚本，CLI 浏览器认证、SSH 管理段和 Remote SSH，文档另有 Windows PowerShell。[安装指南](https://docs.boxd.sh/cli/installation) · [CLI 官网](https://boxd.sh/features/cli) | 明天按候选验证 Linux、Darwin 下载/校验/安装/升级后第一次连接，包含旧 SSH 配置未受影响、改名与删除后别名收敛、IDE 连接、JSON 与非交互。Windows 与安装脚本可用性分别登记。浏览器登录沿用主交接 §4。 |
| Mac 菜单与后台服务 | CLI 自带菜单常驻组件，筛选、终端选择、状态、确认破坏性动作、快捷复制、账户退出。隐藏菜单后 SSH 同步和本机工具继续。[菜单文档](https://docs.boxd.sh/guides/menu-bar) | 现有 App 有模板选机、状态、Terminal.app、Console/Web URL 和电源操作，和 CLI 共用 profile。先真机验分享入口、取消操作、失败反馈、退出后的行为、签名/公证。再排菜单筛选、终端选择、快捷复制、共享登录退出、常驻同步和自有品牌。[AppKit 源码](../../packaging/macos/main.swift) |
| 手机和笔记本的权限桥 | 手机临时密码与 SSH 深链。本机工具登记设备，支持剪贴板、受限只读文件和本机 Chrome profile，用户选择启用。[Mobile](https://docs.boxd.sh/guides/mobile) · [Client utilities](https://docs.boxd.sh/guides/client-utilities) | 已有 Web 终端、托管 SSH 和预览 QR。移动首版只验登录回跳、按次凭据、过期/撤销、iOS/Android 兼容和深链失败提示。本机文件、剪贴板和 CDP 按设备权限另排。 |
| Web 服务首达 | Boxd 默认 URL 可自动选择常见的 Guest 监听端口，并允许显式覆盖，博客描述其服务发现实现。[HTTPS](https://docs.boxd.sh/guides/https) · [服务发现博客](https://boxd.sh/blog/my-kernel-snitches/) | 已有 Machine `web_url`、显式服务发布和 readiness。缺默认 URL 的自动端口发现和选择策略。明天验显式发布、真实就绪与私有访问。若后续加自动建议，按现有 Worker/Guest 观测做，明确授权、歧义、显式覆盖与被动查询不唤醒。 |
| 桌面与 Agent 控制 | 支持 Guest Browser、人工接管和 Computer Use。[桌面博客](https://boxd.sh/blog/anthropic-computer-use-on-a-remote-vm/) · [Computer Use](https://docs.boxd.sh/guides/computer-use) | 已有授权 VNC 和活动保护。验浏览器窗口的真实访问和租约。截图、输入、脚本化浏览器和恢复后的会话绑定作为单独合同。 |
| 自托管分发与运维入口 | 官网和 GitHub 给出版本化签名 bundle、安装器、云端安装、域名委派和 console 登录。GitHub 文档明确不同节点形态、DNS 和遥测边界。[自托管站点](https://boxd.sh/self-hosting/) · [安装器仓库](https://github.com/boxd-sh/self-host) · [自托管 Agent 博客](https://boxd.sh/blog/self-hosted-ai-agents/) | 我们先验 KVM/PVE 的现有安装包、包内 CLI、公开下载、升级回退、分离 apps 域和 Browser OAuth。云模板、常驻升级、租户级运维入口后置。开源 SDK、自动化安装器、可下载运行时是独立交付物。 |

## 4. 官方 GitHub 适配器与生态覆盖

Boxd 官方适配器的价值在于把清理、异常映射和样例一起交付。下面六项在其组织内有公开代码。总表其余合作项目多为合作方实现或指南，按仓库和版本另行核实。未在本候选找到对应的正式发布适配器，先选择一个路由和一个维护方。[官方 GitHub](https://github.com/boxd-sh) · [OSS 指南](https://docs.boxd.sh/use-cases/oss-integrations) · [生态博客](https://boxd.sh/blog/oss-integrations/)

| 官方适配器 | 观察到的交付形态 | 对我们可复用的验收 |
|---|---|---|
| [Pi](https://github.com/boxd-sh/boxd-pi) | 本地 Agent 的 Shell/文件落在远端机，可按会话恢复、Fork。 | 用两次会话和一个子机测工作区、连接恢复、隔离及退出收尾。 |
| [Inspect AI](https://github.com/boxd-sh/boxd-inspect) | Python 评测 sandbox 后端，可承接现有 Docker 评测任务。 | 一份评测样本独占一个 Guest，测超时、文件、取消和回收。 |
| [LangChain Deep Agents](https://github.com/boxd-sh/boxd-langchain) | Deep Agents sandbox backend，提供执行与文件操作，可接已存在的机器。 | 至少一轮文件编辑加命令，验证连接复用与可预测的错误映射。 |
| [Google ADK](https://github.com/boxd-sh/boxd-adk) | 将 VM 操作暴露为 Agent 工具，可指定机器或为会话分配。 | 运行最小工具集合，明确机器创建、工具权限和会话结束后的资源拥有者。 |
| [Convex](https://github.com/boxd-sh/boxd-convex) | 组件将机器和命令状态映射到响应式表。 | 用 operation/event 作为状态来源，断线后恢复游标和状态快照。 |
| [Dify](https://github.com/boxd-sh/boxd-dify) | 可安装的 workflow 工具插件，提供运行命令、文件与机器操作。 | 用测试流程验证凭据作用域、写入未知、工作流取消和删除。 |

X 还宣传了 Pydantic AI、Paperclip、rakazo 和 Agent Swarm。公开指南表明它们分别覆盖工具执行、长时 Agent、桌面恢复和按需 Worker 池。用这些场景选择验收样本，真实容量仍在自己的双物理 Worker 上测。若用 Moyai/Harbor 或其他客户案例做比较，分别记录建机返回、Guest 可执行、评测启动及清理，再记录并发和样本规模。[Pydantic AI](https://docs.boxd.sh/use-cases/oss-integrations/pydantic-ai) · [Paperclip](https://docs.boxd.sh/use-cases/oss-integrations/paperclip) · [rakazo](https://docs.boxd.sh/use-cases/oss-integrations/rakazo) · [Agent Swarm](https://docs.boxd.sh/use-cases/oss-integrations/agent-swarm) · [Moyai 博客](https://boxd.sh/blog/moyai/)

## 5. 明天会上怎么收口

| 顺序 | 工作线 | 交接证据与完成条件 |
|---|---|---|
| 1 | 现有服务发布和客户端交付 | 在主报告冻结同一 SHA、Bundle、CLI 与 SDK 制品。按 J01–J11 验现有入口，再登记 Linux/macOS 下载、普通/IDE SSH、Web 登录、明确的服务就绪。Mac 真机和企业 OAuth 依赖按原计划记录。 |
| 2 | 双 SDK 最短程序 | 同一候选和两份干净安装证明。记录实际具名方法和低层调用，明确哪些仍依赖 CLI。新增封装先写与 HTTP 合同一致的用例，再调整发行文档。 |
| 3 | 两个外围样板 | 登记 GitHub PR Preview 与定时唤醒，分别分配 provider scope/测试仓库和 scheduler/隔离 Worker。其余连接目录、适配器及客户端权限桥按需求排期。 |
| 4 | 对外验收记录 | 每行持续记录负责人、状态、候选 SHA、环境、证据链接、未验证依赖和恢复条件。社交宣传附官方来源。只写我们实际验证的能力与计时。 |

外围新增项沿用主交接既有的发布边界。定时、连接和生态适配的首个样板可在已有平台候选之外独立排期。
