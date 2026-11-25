# 冻结 Runtime Bundle dbe526e 验收

2026-11-25 在隔离的 Linux x86_64 AMD KVM Worker 上完成构建和安装。Bundle 摘要为 `sha256:435b57796237fc3f616bbd1492a39a3aed867428eb0578b59c796fab73091f5b`，本地安装目录为工作区的 `.build/release-dbe526e/installed`。

| 源 | 固定提交 |
| --- | --- |
| smolvm | `dbe526eff7ad5d401267993d5c90bc5451436c8d` |
| libkrun | `ee06d9fd764759e69316e3ee8451bfa61b19b0dc` |
| libkrunfw | `4138851cd3a2c3ace838167556e6ddf855c73445` |

在 Debian 11 的锁定构建环境生成 firmware、libkrun、静态 init、Agent、身份 helper、OCI rootfs、smolvm 和 resourced。libkrun、smolvm 和 resourced 的最高 glibc 符号版本为 2.30。通过源码来源记录、导出、静态 init、库别名、内核配置和 guest 文件一致性检查。安装包包含已校验的源码归档和许可证材料。

## 运行验证

| 验收 | 观测结果 |
| --- | --- |
| 构建产物及安装目录 | 各用分发 wrapper 在两台独立 KVM Guest 内执行探针。两份证据逐字节相同，Guest 内核为 `6.18.60`，时钟为 `kvm-clock`。CPUID、XCR0、设备槽和 KVM 扩展随 Bundle 保存。UID registry 在两次启动后均为 0700。 |
| 验证和安装 | `bundle.py install` 与安装后 `bundle.py verify --expect` 得到同一摘要。安装目录和 wrapper 权限为 0555。 |
| Serve 握手 | 从安装目录启动真实 Serve 并从私有 Unix socket 读到 `runtime-ext/v1`、上述 Bundle 摘要、来源对应的 reader 和运行时实例 ID。七项协议均为版本 1。 |
| Debian System Guest | 用安装包同字节的 libkrun、firmware 和 Agent，在 ext4 根盘运行 systemd 247。`/proc/1/exe` 的内容与发行版 systemd 相同，taskd 报告协议 1 和可用 cgroup v2。Agent 停止后其控制通道不可用，重新启动后恢复 exec，PID 1 和用户进程的 PID 与启动时间保持不变。 |
| System SSH 和容器 | Guest 内完成 OpenSSH 回环命令。Docker 20.10.5 无缓存构建镜像，Compose 1.25.0 重新创建并运行工作负载，返回探针输出和退出码 0。 |
| System K3s | 在离线种子提供的集群上启动 K3s v1.34.13+k3s1，节点进入 Ready。新建 namespace 与 ConfigMap，启动 Pod 并读取探针日志。该轮的 CoreDNS Pod 状态包含 Unknown，DNS 功能留待独立验收。 |

候选清单发布的引导配置为 `oci-v1`。System 验证使用单独准备的 Debian 盘和 block-root 引导探针。本轮的 System 范围覆盖启动、服务和工作负载，完整状态 Fork、Hibernate 和身份恢复有各自的验收项。

构建、安装、OCI 和握手记录位于 `.build/release-dbe526e`。System 的命令输出和输入摘要在 `system-smoke` 下。早期 `5f5971f` 候选的 UID registry 0700 错误保存在 `.build/release-5f5971f/oci-runtime-smoke.log`。修正后的冻结候选通过了相同检查。首次握手启动因 socket 目录已容纳其他文件被拒绝，使用独立目录后验证通过，首次结果保存在 `installed-serve-original-path.log`。
