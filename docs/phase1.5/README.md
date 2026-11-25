# Phase 1.5 · Sandbox Console

本机部署版本为 0.1.7。控制台沿用 platformd 的 Go 静态嵌入，字体、图标、JavaScript、样式和源码下载均由同一服务提供。管理入口沿用 [本机 8443](http://127.0.0.1:8443)。

## 移植和交互

直接移植了 Daytona v0.190.0 的页面标题、侧栏菜单、表格、按钮、弹窗、下拉菜单、加载骨架、空状态和双主题变量。源提交为 `01c502bb1f1ff8f2885d0cd490e043736083dca8`。本项目负责导航、认证、API 适配、实时事件和实例操作。下游修改和第三方声明见[组件对照及许可证](../../web/licenses/README.md)。[Daytona dashboard 源码](https://github.com/daytonaio/daytona/tree/v0.190.0/apps/dashboard)

机器列表采用状态分段筛选和行内快捷操作。点击终端直接打开 root PTY，详情里的 Terminal 标签也能重连已有会话或自选命令。Branches 从实例递归读取相连的依赖，显示来源、存储依赖及后端报告的释放状态。Snapshots 可跨实例查找和恢复，Tasks 可直接进入对应任务的日志，Networking 可进入对应实例管理端口和访问方式。操作回执链接到持久的 Operations 记录。

布局参考了用户提供的 boxd Dashboard、Machines、Disks 和 Settings 登录后截图。另用 Chromium 查看了官网 Console、Fork、Checkpoint、Domains 的公开演示及 CLI 和开发环境页面。开发参考截图保存在本机工作区的 `.build/phase15/boxd/`。用户提供的账号画面仅用于比对。[boxd 官网演示](https://boxd.sh/) · [CLI](https://boxd.sh/features/cli) · [开发工作区](https://boxd.sh/use-cases/dev-environments/)

新增的服务端接口为只读 `GET /v1/snapshots`，复用现有快照目录、权限判断和游标分页，支持来源实例已删除的快照。Go 静态处理器对固定 URL 的源码包和许可证重新验证缓存。smolvm 保持 1.37.1，数据库 schema 保持 2。

## 本机验收

在独立测试注册表上，以实际 platformd、smolvm 和已验证的 Debian 12 模板运行 Chromium。所有截图使用平台返回的数据。

| 交互 | 实际结果 |
| --- | --- |
| 创建、终端、Files | 表单创建并启动实例，列表快捷按钮打开原生 PTY，执行命令得到 root、/workspace、Linux。浏览器能打开 Guest 目录。 |
| Live Fork | 一个标签页持续连接父机 PTY，另一个标签页提交 Fork。子机运行后，原 PTY 仍能执行命令。 |
| Independent Clone、分支树 | 独立克隆显示逻辑来源。实时分支另有 active 原生存储依赖，两类子机均可从树中打开。 |
| 快照、恢复、休眠 | 活动测试 shell 触发 clean checkpoint 的真实拒绝原因。退出 shell 后捕获成功，从全局目录恢复到新实例并执行完整状态休眠。验收时曾以错误的服务账号访问本机 status socket，Operations 显示 recovery_required。切回正确账号后通过 Reconcile 完成了原恢复操作。 |
| Tasks、Networking、SSH | 持久任务输出可从全局任务表打开。发布 SSH 服务后，可从全局网络表进入管理页，也可从机器菜单查看连接命令。 |
| 权限和分页 | 两页快照无重复；限定到来源实例的只读 token 只能看到对应快照；另一实例的只读 token 得到空列表；tunnel token 得到 HTTP 403。 |
| 主题、窄屏、部署 | 浅色主题刷新后保留，390px 宽度下侧栏可操作。升级后的 8443 实例通过浏览器登录、读取模板和机器、退出，runtime_ready 为 true。 |

`npm run build` 与 `go test ./...` 通过。从控制台下载对应源码，在独立目录运行 `cd web && npm ci && npm run build`，随后在根目录执行 `go build ./cmd/platformd` 成功；重新生成的源码包 SHA-256 与下载内容一致。安装前备份已由 sandboxctl 校验。

## 截图

[机器列表](screenshots/machines.png) · [创建实例](screenshots/create-machine.png) · [浅色实例详情](screenshots/overview-light.png) · [原生终端](screenshots/terminal.png) · [快照](screenshots/snapshots.png) · [分支树](screenshots/branches.png)
