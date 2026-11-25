# Deep 源码检出与 Worker 隔离预检

## 锁定源码

`baseline-lock.json` 固定 Git 提交、smolvm 两个子模块 gitlink、固件的观察提交及其 tree。以下命令从平台仓库执行，检出目录应为独立的新目录，父目录应已存在。工具需要 Git 和 Python 3.10+。

```bash
python3 packaging/deep/source-checkout.py checkout \
  --root /var/tmp/smolvm-deep-baseline --https-source upstream
python3 packaging/deep/source-checkout.py verify \
  --root /var/tmp/smolvm-deep-baseline
```

`upstream` 从平台仓库和 `smol-machines` 的三个锁定 HTTPS 仓库获取基线提交。`fork` 方式从锁中的个人 HTTPS 仓库获取相同对象，遇到私有仓库、未发布对象或凭据缺失即停止。四个工作树保留各自的 HTTPS `origin`。有定制上游的仓库同时保留 `upstream`。`verify` 校验实际 Git 根路径、remote、HEAD、tree 和工作树状态，从父提交的 `.gitmodules` 和 gitlink 解析两个依赖路径，并独立验证子模块 HEAD。固件观察提交的 tree 也必须与锁定值一致。

离线模式接受四个由上述来源产生的本地 Git 镜像或 bundle，文件或目录名依次为 `platform-smolvm.git`、`smolvm.git`、`libkrun.git` 和 `libkrunfw.git`。固件镜像需要同时包含锁定提交和同树的观察提交。

```bash
python3 packaging/deep/source-checkout.py checkout \
  --root /var/tmp/smolvm-deep-offline --mirror-dir /media/deep-git-mirrors
python3 packaging/deep/source-checkout.py verify \
  --root /var/tmp/smolvm-deep-offline
```

检出目录已出现即拒绝覆盖。中断后可保留原目录排查，并用新目录重新运行。源码检出保留 Git LFS 指针并在 JSON 的 `lfs_pointers_in_worktree` 中报告仍待获取的文件。需要发布所使用的 LFS 内容时，在独立构建输入中获取和核对实际内容。运行时库以构建包的来源和摘要为准。

首次准备下游分支时，应先验证干净基线，再用以下命令创建四个本地 `deep-runtime/main` 分支并生成 smolvm 的两项 `.gitmodules` URL 修改。先审查 `git diff` 和 `git ls-tree HEAD`，再将修改提交到下游分支。

```bash
python3 packaging/deep/source-checkout.py prepare-downstream \
  --root /var/tmp/smolvm-deep-baseline
git -C /var/tmp/smolvm-deep-baseline/smolvm diff -- .gitmodules
git -C /var/tmp/smolvm-deep-baseline/smolvm ls-tree HEAD libkrun libkrunfw
```

工具检查现有分支并拒绝重复创建，保留 Git 工作树供审查。基线验证针对固定的干净基线。后续构建应使用候选 Bundle 的实际提交、tree 和 gitlink 锁。

### 2026-11-25 源码核对

| 仓库 | 固定提交 | 实际 tree |
| --- | --- | --- |
| platform-smolvm | `582a6f141fedd7381acd54ff2cb11ae7a6e0f899` | `65c3bab2af498eda80042719d54ab2d8e457753b` |
| smolvm | `5bd7c2f8e8e2d29869226a91f766bf4147b5e693` | `5f8eb468874f768a0f24ec450fafd4848d1d97ed` |
| libkrun | `36b0846191bf8dbfc044c98cf43922c2f1f221e4` | `0881efe9ef8ebf10e56d98c533b49c171857cb21` |
| libkrunfw | `168e68a068c43a0fd48b71d1f52ca0081d079803` | `b30cd9d5af81e0c8ac10cfcf3f79b05d57adc327` |

用实际 Git 对象完成四仓库的独立离线检出、复核和下游准备。`libkrunfw` 观察提交 `2f12ddce482f0956d7e9d4695e0f8a1bc38a1c9a` 的 tree 与固定提交一致。个人 smolvm 的 HTTPS HEAD 已观测到 `5bd7c2f`。两个个人依赖 URL 的匿名查询需要凭据，Fork 存在性和父仓库关系仍待凭据核验。可用的离线源码来自计划指定的定制上游。smolvm 固定提交包含 19 个已提交的 Git LFS 指针，其中 Linux x86_64 有 6 个。其余三个固定提交未发现 Git LFS 指针。

## 首次安装前的只读隔离预检

以 `packaging/deep/isolation.example.json` 为模板保存实际 Worker 清单。为 `deep`、`official` 和 `cocoon` 声明在场状态，填入各路线的真实路径、socket 根目录、服务和 slice、监听地址与端口范围、IP 池。路线确实未部署时才能设 `present: false`。示例端口、CIDR 和路线状态需按现场重审。把未显示为宿主 route 的 PVE 私网、Docker/k3s Pod 或 Service CIDR、其他 netns 的保留地址写入 `host_address_reservations`。工具内置 PVE `10.10.0.0/24`、Tailscale `100.64.0.0/10`、Mihomo `198.18.0.0/15` 的避让规则。

```bash
python3 packaging/deep/preflight.py --inventory /path/to/deep-isolation.json --inventory-only
python3 packaging/deep/preflight.py --inventory /path/to/deep-isolation.json
```

静态模式比对声明的目录、服务命名、端口和 CIDR。路径检查会解析符号链接并比对 mount 和 inode，以处理同一文件系统对象的多个本地路径。`smolvmdeep.slice` 是独立的顶层 slice。`smolvm-deep.slice` 会落到 `smolvm.slice` 之下。主机模式额外读取 IPv4/IPv6 路由和接口网段、TCP/UDP 和 Unix socket、已加载和已安装的 systemd 单元、KVM 设备权限、cgroup v2、内存 PSI，以及各目录最近现存父路径的可用字节和 inode。Linux 双栈通配监听按可能占用 IPv4 空间检查。实际 Worker 盘点依赖当前挂载、网络命名空间和 systemd 可见性，应从与计划部署一致的宿主环境运行。

| 退出码 | JSON 状态 | 含义 |
| --- | --- | --- |
| `0` | `clear` | 所选范围内各项已检查且无冲突 |
| `1` | `conflict` | 至少一项明确冲突，`unknown` 同时保留任何缺失证据 |
| `2` | `unknown` | 清单无效、现场信息缺失或宿主条件未核实 |

现场命令均为读取。结果是首次安装前的快照。安装时仍需再次确认端口和地址预留、配置归属和磁盘挂载。实际 KVM 功能与 VMM scope 的运行状态由后续 Worker 门验证。

当前开发容器对示例清单的静态结果为 `clear`。现场结果为 `unknown`，netlink 和 systemd 系统总线访问受限，且没有 `/dev/kvm`。已读取 cgroup v2 控制器、内存 PSI 和目标路径容量。生产 Worker 的现场隔离仍待带实际清单复核。

```bash
python3 -m unittest discover -s packaging/deep -p 'test_wp00.py' -v
```
