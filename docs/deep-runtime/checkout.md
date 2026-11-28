# 源码与 Worker 预检

## 开发目录

从工作区的原始仓库目录继续。以下提交是 2026-11-28 验证与合并记录使用的源码起点。应用修订时记录实际 HEAD、Git 子模块和待发布制品摘要。

| 工作区相对路径 | 分支 | 已核对的源码起点 |
| --- | --- | --- |
| `platform-smolvm/` | `polish/integration` | `69a42f4cbba0403f0add0f5131f12cf3ae029cc3` |
| `smolvm/` | `polish/runtime-candidate` | `eace7d5918eb491c954b6c5f864d3cd06c22f156` |
| `smolvm/libkrun/` | `feat/kernel-cow-runtime` | `6aef96de632c39f81727ae664e0fda254c356209` |
| `smolvm/libkrunfw/` | `deep-runtime/main` | `4d5852a59f9c8bc75780544d0fe93b5015343a32` |
| `kernel-cow-k1/linux-patches/` | `feat/kernel-cow-runtime` | `a4f76364095f63712299dd65603130c83d17ec14` |

在工作区顶层核对当前提交和配套子模块。

```sh
git -C platform-smolvm status --short --branch
git -C platform-smolvm log -1 --format='%H %s'
git -C smolvm status --short --branch
git -C smolvm log -1 --format='%H %s'
git -C smolvm submodule status
git -C kernel-cow-k1/linux-patches log -1 --format='%H %s'
```

平台合并源码同时包含 Kernel CoW 与多 Worker。Runtime 的 gitlink 固定 libkrun 和固件。Worker 启动内核与业务 Guest 内核分别取证。开发 Bundle、Worker 内核 canary 和当前源码按各自版本记录。[运行时制品合同](../../packaging/deep/bundle.md) · [内核 CoW 验收版本](kernel-cow-closeout-2026-11-28.md)

## 首次安装前的只读隔离预检

以 [隔离清单样例](../../packaging/deep/isolation.example.json) 为模板，填写实际已部署路线、路径、socket 根目录、systemd unit/slice、监听端口和 IP 池。从平台仓库运行。

```sh
python3 packaging/deep/preflight.py --inventory "$WORKER_ISOLATION_INVENTORY" --inventory-only
python3 packaging/deep/preflight.py --inventory "$WORKER_ISOLATION_INVENTORY"
```

静态模式比较清单中的路径、service/slice、监听端口和地址池。主机模式另读实际进程、挂载、监听器和 IPv4/IPv6 路由。退出码 0 为 `clear`，1 为 `conflict`，2 为 `unknown`。在测试 Worker 上另外核对 KVM、有效 CPU/RAM、磁盘、模板、Bundle、reader 和端口隔离，再执行[发行包的只读安装计划](../../packaging/install/README.md)。
