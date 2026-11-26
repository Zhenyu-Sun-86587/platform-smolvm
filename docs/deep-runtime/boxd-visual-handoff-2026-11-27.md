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
