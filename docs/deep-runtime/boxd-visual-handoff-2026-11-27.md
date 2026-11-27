# 外观与首次使用体验交接

本轮对照 Boxd 官网、登录入口、官方博客和视频、X 发布演示、已有前端截图及 CLI/Mac 原型。平台源码以 `polish/integration@4e1ba47` 为候选，历史截图和 `final-5c153be` 的 PTY 实录保留各自版本。视觉方向是统一自有品牌，保留高频任务的效率，并调整导航、首屏、详情页层级等最能形成产品辨识度的部分。[生产交接](production-handoff-2026-11-27.md) · [外围功能验收](boxd-peripheral-handoff-2026-11-27.md)

## 看到了什么

| 样本 | 可复用的体验原则 | 当前差距 |
|---|---|---|
| 官网、自托管与博客 | 首页把最短安装命令放在首屏，再用一段可切换的 CLI/Console 场景介绍产品；博客用有一致风格的插图、图示和可复制命令说明具体工作流。X 的 60 秒片把安装、建机、访问、Fork 和休眠串成一个故事。[官网](https://boxd.sh/) · [自托管页](https://boxd.sh/self-hosting/) · [产品博客](https://boxd.sh/blog/boxd-101/) · [X 发布演示](https://x.com/boxd_sh/status/2104537788859216343) · [发布海报存档](https://posts.design/boxd-sh-boxd-in-60-seconds-2026-09-28) | 仓库有运维安装说明和 Console。缺用户可到达的下载/入门页，以及从真实候选录制、展示实际操作阶段的短片。先做清楚可用的入口，插图和推广资产可独立迭代。 |
| 登录和首次动作 | Boxd 公开登录页把 CLI 下载、登录选择和产品说明放在同一入口。已登录 Console 又有明确的下一步。[Boxd 登录入口](https://app.boxd.sh/) | 我们有账号密码登录、CLI 交互 token 引导和 Dashboard。需要统一品牌和链接，把 CLI 下载、浏览器认证结果、首台机器与第一个连接方式串起来。登录的拒绝、取消、过期和旧 token 路径分别呈现。 |
| Web 工作台与移动布局 | 参考样本用轻量层级组织密集机器行，详情将高频连接、状态和更多设置分层。移动端改成卡片和底部弹层，操作菜单与桌面有各自的密度。自动化案例用流程图解释触发、运行与连接。[官方自动化案例](https://boxd.sh/blog/automations-that-sleep/) · [前端历史并排样本](/home/dev/workspace/deepfork-smolvm/.build/polish-frontend/evidence/final/index.md) | 已有侧栏、命令面板、抽屉、浅/深主题及移动安全区。并排样本中 Dashboard、列表和创建浮层的构图、暖色按钮和块面位置仍很接近参考。优先用自有导航与工作区布局、主操作和状态层级让产品在结构上可辨识。保留键盘、路由和真实能力门控。 |
| 状态、终端、桌面与空态 | 参考的价值是把能否连接和下一步操作放在离资源最近的地方，浏览器桌面提供明确加载与接管。日志和等待态各自有出口。[桌面指南](https://docs.boxd.sh/guides/desktop) · [自动化作业界面](https://docs.boxd.sh/guides/automations/jobs) | 历史实拍已分 Guest Ready、Service Unknown、指标 Partial 等状态。部分层级仍偏密，同页既有运行徽章又有详情说明。交接要求把 Operation 受理、Guest 可执行、Service 可访问、配置与观察做统一的文字和视觉层级，并给 Unknown/失败/取消真实的后续操作。Agents 等预览场景以真实生产资格决定入口。 |
| CLI 安装与开场 | 官方安装录像展示版本、下载、校验、路径和完成后下一条命令。安装日志与 REPL 的短开场分别解决信任和引导问题。[CLI 安装文档](https://docs.boxd.sh/cli/installation) · [官方安装录像](https://mintcdn.com/azin/AlEw94GJXGsEfPlY/videos/install-cli.mp4) | 我们已有首启引导、补全、静态回退和可替换字样。PTY 实录完整开场约 0.62 秒，按键能跳过且光标恢复，现有默认 fallback 是 `Sandbox Platform` 文本。需要发布自有终端字样和安装输出层级，同时验窄屏、浅/深终端、无色、非 TTY、JSON、减少动态和中途退出。[CLI 开场代码](../../cmd/sandboxctl/welcome_brand.go) · [PTY 原型记录](/home/dev/workspace/deepfork-smolvm/.build/polish-20261126/cli-onboarding-macos/final-5c153be/prototype/README.md) |
| Mac App 与状态区 | 官方菜单录像展示欢迎、建机、机器列表、打开终端与网页。Apple 菜单栏规范让状态图使用模板图适配高亮和浅/深外观。[Boxd 菜单指南](https://docs.boxd.sh/guides/menu-bar) · [官方菜单录像](https://mintcdn.com/azin/Gc7WdxwqyVxP34FT/videos/boxd-menu-bar.mp4) · [Apple 菜单栏规范](https://developer.apple.com/design/human-interface-guidelines/the-menu-bar) | AppKit 原型已有 `NSMenu`、菜单项状态、连接和电源操作，外置图标会以模板图加载，默认使用系统 `shippingbox`。构建脚本与 Info.plist 尚未装配正式 App 图标资源。需要区分 Finder/安装包的 App 图标和菜单栏单色模板，检查长名称、窄菜单、选中态、错误及键盘/VoiceOver。Mac 真机未验证。[AppKit](../../packaging/macos/main.swift) · [Info.plist](../../packaging/macos/Info.plist) · [Mac 构建](../../packaging/macos/build.sh) |

官方安装录像与菜单录像的关键帧已复看。X 原页仍受访问限制，海报和索引只支持传播方式观察。旧 Web 对照包含 46 个编号场景与平板粗指针补图，覆盖固定 fixture 及另存的真实 KVM Console 实拍。现有证据可复用来选场景，最终签字使用同一个部署候选的新产物。[历史截图索引](/home/dev/workspace/deepfork-smolvm/.build/polish-frontend/evidence/final/index.md) · [参考素材来源与哈希](/home/dev/workspace/deepfork-smolvm/.build/polish-20261126/cli-onboarding-macos/boxd-reference/sources.json)

## 明天交什么

1. **确认可分发的自有品牌资产。** 交付主标志、文字标、小尺寸 favicon、App 图标、菜单栏模板和终端字样。提出两套可快速取舍的自有视觉方向，至少在主强调色、导航构图、标题与状态层级上形成风格。明确字体、图标、图形和媒体来源及许可。Web 当前为 Inter/JetBrains Mono 和通用立方体标识，HTML 入口未声明产品 favicon。先集中名称、标识与语义色入口，再落资产及浅/深主题检查。[Web 品牌入口](../../web/src/layout.tsx) · [Web CSS](../../web/src/style.css) · [Web HTML](../../web/index.html)
2. **同一条首次使用路线。** 下载及校验 → 登录 → 选择模板建机 → 等 Guest → 打开 SSH、终端或实际已就绪的网页。Web 和 CLI 的截图/短录屏用发布候选，从干净用户目录操作，页间按钮、复制命令、服务 URL 和错误提示用实际值。Mac 真机后补对应画面。演示分别标注受理和首次连接，耗时沿用生产交接 §6。
3. **先改影响识别和可用性的页面。** 先过登录/下载、Dashboard 与机器列表、建机弹层、详情/连接、浏览器终端。最小 Web 矩阵为桌面浅/深、390×844 手机触屏、768×1024 平板细/粗指针，再验 200% 缩放、超长名称、键盘焦点与弹层关闭恢复、移动软键盘和减少动态。普通文字、主要控件和小状态标签检查对比度，信息用文字或符号辅助颜色。以 WCAG 2.2 AA 的相关单项做检查，结果按场景记录。[WCAG 2.2](https://www.w3.org/TR/WCAG22/)
4. **复验开场动画与 App。** CLI 保持当前约 0.6 秒原型作为节奏基线，用自有字样，首启按键立即跳过，减少动态、超窄或不适合动画的终端用静态版。脚本/JSON 输出沿用现有协议。Mac 用签名 App 检查 App 图标、浅色/深色菜单、选中态、长列表、VoiceOver、从菜单到正确 profile 和实际 SSH/网址的全过程。若只有跨编译和 Swift 语法证据，按未实机验证登记。[CLI 测试](../../cmd/sandboxctl/onboarding_test.go) · [Mac 使用入口](../../packaging/macos/README.md)
5. **发布和证据分账。** 根据实际能力露出产品导航和下载项，记录自有素材及发布包内容。本地原参考、账号画面和素材保留作研究输入。开发方负责固定 SHA/Bundle、覆盖日志、截图与可定位的失败步骤，人工只确认标识、动效节奏和最短路径的主观观感。

验收表每行带候选 SHA、设备/浏览器、视口与指针、主题和减少动态、数据来源、截图或视频、结果及仍缺的条件。优先收一组 Web/CLI/Mac 的连续证据。当前已核查历史样本和源码，未做新制品的视觉复验或 macOS 真机测试。

## 第二轮对照：同一台机器如何在不同屏幕间接续

以平台源码 `15b541d` 复核。历史截图 41–46 的两列分别是 Boxd 参考与我们的 Chromium 实拍。390px 机器列表已能把操作收进卡片，建机也已使用底部面板。两边最相似的是首页问候语下方的搜索、成组快捷按钮和下面的机器卡片。品牌替换优先改这里。可先给同一台机器做两版首页草图：一版以“上次连接的工作区”为首项，另一版以“机器列表 + 最近操作”为首项。用首台机器和二十台机器两组数据比较找到目标、判断状态、复制连接命令所需的操作数。[手机和平板并排截图](../../../../.build/polish-frontend/evidence/final/pairs/41-46.jpg)

| 接触点 | 公开证据与当前实现 | 明天怎样验 |
|---|---|---|
| 桌面交给手机 | Boxd 在电脑上展示一张只含机器页面链接的二维码，手机登录后再选 SSH 客户端或看连接参数。我们的连接弹窗中，编辑器图标与二维码位于 `ConnectionPreview`，只在编号 30 的演示场景渲染。手机尺寸网页已具备，扫码交接仍缺生产入口。[Boxd 手机指南](https://docs.boxd.sh/guides/mobile) · [连接弹窗](../../web/src/workspace-actions.tsx) | 扫码应落到同一台机器，换账号要重验授权，码内仅放普通页面 URL。分别在真实 iOS Safari 和 Android Chrome 验证取消、返回、未登录、权限已收回。当前托管 SSH 采用每次会话公钥授权，手机连接方案仍需单独确定。 |
| 浏览器桌面 | Boxd 指南区分按窗口调整分辨率、Agent 固定分辨率、文件拖放的逐项进度、接管与重连。我们的生产 Desktop 使用 noVNC 缩放并接收连接事件；带 ⌘B 的应用启动器属于 `DesktopFixture`。编号 34、39、40 能说明设计，不能证明已连到 Guest 应用。[Boxd 桌面指南](https://docs.boxd.sh/guides/desktop) · [生产和演示组件](../../web/src/Desktop.tsx) | 在真实 VNC 会话里分别录“连接中 → 已连接”“另一窗口接管或被拒”“真实断连 → 用户重连”。把当前机器名和连接状态留在可发现的位置。拖目录、剪贴板、远端调分辨率按独立能力验收后再开放。 |
| Mac 的常驻菜单 | 官方录像里从菜单中新建机器，打开终端，最后在浏览器看到 nginx 页面。指南另列过滤、终端选择、按机器状态显示动作及有影响操作的确认。我们的 AppKit 已有运行数、刷新、机器子菜单和电源确认，终端入口指定 Apple Terminal。长列表筛选及终端偏好是可见差异。[官方菜单指南及演示](https://docs.boxd.sh/guides/menu-bar) · [菜单实现](../../packaging/macos/main.swift) | 用 0、1、20 台机器，在长名称与相近名称下用键盘选目标。弹出 Hibernate 确认后取消，核对零请求；确认后跟踪原 operation。检查离线或刷新出错时，上次看到的运行数是否会被误读为当前值。 |
| 官网与演示片 | Boxd 的 Fork 功能页列出左右分屏脚本，左侧是父机服务，右侧是 Fork 后访问子机。页面这段标有 “GIF Needed”，只能引用为分镜提议。X 原帖此次返回 403。已有官方菜单录像可以核实菜单工作流。[Fork 功能页](https://boxd.sh/features/forking/) · [官方菜单录像](https://mintcdn.com/azin/Gc7WdxwqyVxP34FT/videos/boxd-menu-bar.mp4) | 自己录一镜到底：保留父机日志、子机名、operation ID 和子机首次响应。字幕分列“已受理”“Guest 已执行”“服务已响应”，用同一时钟。不用开场动效的时长或操作完成时刻替代服务就绪时间。 |
| 集成市场的小图与文案 | 官方 GitHub 的 Dify 插件清单单独声明了图标、英中文名称/说明、隐私文件及 `verified: false`。产品会出现在第三方工具选择器、安装页和授权页，自有品牌资产也要覆盖小图和一行文字。[官方插件清单](https://github.com/boxd-sh/boxd-dify/blob/main/manifest.yaml) | 等首个适配器有真实安装包，再在宿主应用截取搜索卡、凭据页和失败状态。用自有图标检查浅/深底、小尺寸和中文截断。仅展示宿主实际给出的验证标记。 |

最短签字路线是从同一版本下载，首次打开 CLI，用 Mac 菜单打开刚建的机器，再从浏览器回到同一台机器。每一步写下用户此刻能看到的名称、账号或平台地址、状态与下一步动作。三处都能认出“这是哪台机器、是否能连”时，再录对外短片。上表新增项是源码与公开资料的审查结论；当前尚无该路线的真机联测结果。

## 登录页实拍

用 Chromium 151.0.7922.34 加载本仓库已提交的生产版 Web 静态文件，`web/dist` Git tree 为 `5e0a46fae0d3c9425fe37256d2615445e66a48d5`。本地端点只返回未登录状态，未提交用户名或密码。下面两张是我们自己的页面，均为 390×844，按一次 Tab 后截取；密码框外侧的描边是键盘焦点。[深色实拍](evidence/visual-2026-11-27/login-dark.png) · [浅色实拍](evidence/visual-2026-11-27/login-light.png)

| 检查 | 结果 |
|---|---|
| 布局与焦点 | 320×568、390×844、1280×800 各测浅/深两套。六种组合均无页面横向溢出；初始焦点在用户名，第一次 Tab 到密码。移动输入文字为 16px，桌面为 13px，提交按钮均高 35px。[六组浏览器读数](evidence/visual-2026-11-27/login.json) |
| 首次到达能做什么 | 画面中心是一张登录卡，品牌、用户名、密码、登录按钮及 Source / Licenses 可见；此页没有 CLI 下载、Mac 下载或浏览器 OAuth 入口。Boxd 的公开入口同时呈现 CLI 安装和账号登录。我们的首版可在登录卡旁增加一个清楚的“下载客户端”入口，再按部署结果展示平台、架构、版本与校验信息。[Boxd 公开登录页](https://app.boxd.sh/) |
| 改版时保留什么 | 保留小屏完整可见的表单和键盘焦点。先用正式字标替换通用立方体，让同一标识出现在下载页、终端静态开场和 Mac 菜单。下载链接连上实际制品、OAuth 回调可完成后，再录首用演示。 |
