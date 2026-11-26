# f2df146 候选包的授权范围验收

2026-11-26 在同一台 Linux x86_64 KVM Worker 上构建并安装 OCI 候选，使用独立的测量 Bundle 验证自有 Debian System 和服务。安装件与测量件分别保有自己的摘要。

| 构建来源 | 固定值 |
| --- | --- |
| smolvm | `f2df146fd705575294ca121ef7b843377aaf4a36` |
| libkrun | `0d0255383f4378aca5be7bf19629330c08fcaf8e` |
| libkrunfw | `4d5852a59f9c8bc75780544d0fe93b5015343a32` |
| 平台 | `83b3d297e785a7c67c68c94f3f51d475481c4cb8` |
| 构建锁 | `sha256:91393f57124747ed113ab31b87a86ed9a2c20895a29edd96d75a95e264d07a92` |
| 工具链锁 | `sha256:32943b623419022f5fba0908602962791d5aa5a214a67503eec9c8f85992433f` |
| 构建回执 | `sha256:c41d1ef5a0e49afd56b8ef272e8355c588030ebcb68041e6d62d1942945cf01c` |
| Debian System 盘 | 805306368 字节，`sha256:4b5ba5301eac04a398f50fb5be2f969c6e793845d249303783dc2f69a3a3c23f` |

运行库与固件从锁定的三个源码树构建。三个源码归档解压后的 tar 字节与对应 Git 归档一致。构建检查记录位于 `.build/release-f2df146/`。

## 安装验证

OCI 安装目录为 `/var/tmp/deepfork-smolvm-build-cache/release-f2df146/installed-authorized`，Bundle 摘要为 `sha256:b79afe38fceb622c70cb2cc6085a22910aa1ca9dab304ab4b99a1a0ba8029e67`。打包前后分别启动两台 OCI Guest，测量记录逐字节一致，摘要为 `sha256:2fc0794b71020b27b12fbe431b17945398fdff1e061c5de711f820a7de720b0a`。安装后的 Serve 与 Worker 握手记录摘要为 `sha256:5c8f3d17c862b6de745ed79dacfc743ed2912b1b259b3929462d7f12f9c94e43`。本件声明 OCI 启动。时钟资料采用 `RuntimeClockContract/2`，实测操作列为 `oci-boot`。

## System 与私网测量

独立测量 Bundle 摘要为 `sha256:1d79bf8139035f924f8132fe58837686d6da9a0c72bc2c3ee2f32829882bedff`。使用 `measure-managed.py --authorized-only` 完成五个实机用例，记录摘要为 `sha256:888d92cf5e924f47497be7eaf942f1296bf9d5ccf974fd9c9d34132bb7947c60`。可在 `.build/release-f2df146/authorized-v4-core/` 核对五份原始日志和记录。

| 用例 | 实际观测 |
| --- | --- |
| System 冷启动 | Systemd 启动，Guest Agent 读取初始执行的 RAM 标记。 |
| System Fork | 两个独立子进程生成不同的执行 ID，父机及两台子机读到同一捕获点的 RAM 标记。 |
| System Restore | 通过公开 API 使用 `clone_conflicts` 从检查点恢复出另一实例。父机 Hibernate 后显式启动，同一逻辑实例在下一代恢复原 RAM 标记。 |
| Native 和 FixedInternal | 一个方向一个用例。两路并发合法私网 TCP 请求共用一个 Worker 恢复操作，恢复后保留私网地址与 attachment，Guest 各记录一次业务 ID。 |

同一 e2e 夹具只启动自有 Guest，私网规则仅授权本轮源 VM 与端口。历史九组资格未发布。旧全组的两种宿主失败都保留了原始输出，包括私有挂载清理失败及主机内存准入返回 503。本轮范围排除了涉及利用、越权、身份伪造和未授权探测的子项。授权测量使用 `RuntimeManagedAcceptance/4`，需要能读取该记录的新 Serve。上述五项记录用于量测，冻结的 f2df146 安装件只提供 OCI。

同一 f2df146 测量 Bundle 随后跑完了独立的 `--authorized-lifecycle` 组。Owner 提交的 `keep_running` 由 Worker 确认至版本 2，撤回后为版本 3。公开 Standby 与授权租约唤醒使用同一执行和第 1 代，Guest 保留 RAM 标记。公开 Stop 后 Worker 无活动 incarnation，显式冷启动以第 2 代读回根盘标记。`.build/release-f2df146/authorized-v4-lifecycle/` 保存原记录 `9501abbce0bcc52675dd22328ff1a80ce7fa4d3f0aaccadec2d1f397692a2f18` 和完整日志 `4f1c0ad60792bc8eb63bfb173451689e12ea913e5e042ae065c0f45bea22fdd4`。

## A21 授权首访与业务送达

各项使用同一测量 Bundle。公开 API 创建的自有服务先 Hibernate，两个同时发出的流式请求只触发一个 Worker wake，恢复保存时的捕获 ID 和逻辑实例，执行代际从 1 升至 2。业务回执由 Guest 写入，分别核对请求标识和出现次数。

| 入口 | 结果 | 原始日志及 SHA-256 |
| --- | --- | --- |
| HTTPS 网关上的 HTTP | 两个有 scope 的请求各送达一次。第三笔 Guest 已记录业务后主动关闭连接，网关返回 502 和 `may_have_started`，Guest 对该业务只留一条回执。 | `authorized-a21-http.log`，`07bcf39e56f3363fd510ee5c596829cc7d064f3f3a37663f1a7a8943a453be95` |
| TCP-mTLS | 平台公开入口签发仅限当前服务的证书。修复 wake 内服务证书的 scope 复验后，两条 TLS 连接各送达一次。平台二进制为 `cd7a18ac87bcba5a710fc467d6bd05e204320f5d49de4406abaca0ebb8134114`。 | `authorized-a21-tcp-mtls-native-v2.log`，`a9ef9bdd4abf1687d66cee85e57c8519264c5a6687829ff07b6a5adde3ff57a9` |
| SSH | 两个 OpenSSH 进程经 `sandboxctl ssh-proxy` 使用公开注册的密钥和严格主机密钥校验。修复后网关在持有 Worker hold 时等待新 epoch 的主机密钥，每条命令执行一次。平台二进制为 `7a1c13411b276606a17c1564c4279f4117fd5f403f45f30d8d6582458bc36725`。 | `authorized-a21-ssh-native-v3.log`，`17489bf19e0e9667b1d677054d862ea6908f15c60e4c08050ba57e1f026b9739` |
| 直接私网 TCP | Native 与 FixedInternal 各作一次目标，两笔有业务 ID 的并发请求各送达一次。 | `authorized-v4-core/managed/network-native.log`，`ddc76ae17b36eebf141f1dec7044db288724c836932085d1d30f6f34a0e13e84`。`network-fixed.log`，`93029dc11c769b76c8546335977a6599d6e2e58016b7cf9bc469098cadf8a386`。 |
| 直接私网 UDP | 两个目标模式分别提交一个合规短数据报，每次业务标识只在 Guest 出现一次。 | `authorized-a21-private-udp-native.log`，`d423cfe8476df04164f4895daa677e5042f28f663877ba4a186f6b541febcc19`。`authorized-a21-private-udp-fixed_internal.log`，`ea91476ec51e33701dcda5989b0876a75f2394e9b86addc1d54efb492871640a`。 |

原始日志位于 `.build/release-f2df146/`。使用 `tests/integration/authorized_services_kvm.py` 的对应 `--case` 可复跑，传入测量 Bundle、匹配的 Debian 磁盘和上表指定的平台二进制。队列容量上限和其他协议的半关闭送达语义需要独立实测。
