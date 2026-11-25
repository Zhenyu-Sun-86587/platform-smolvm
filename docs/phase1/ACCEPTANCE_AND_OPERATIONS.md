# 第一阶段验收与运维

> 2026-11-25 修订版：默认 Guest root；Live Fork 仅使用原生实时复制，不处理或提供应用安全点，活动工作负载不构成额外准入限制。Independent Clone/同逻辑 recovery 的既有捕获策略保持独立。

<a id="status"></a>
## 1. 文档状态与使用方法

**本文是实施阶段要执行的验收标准与运行手册，本轮没有实施平台、部署服务或执行这些新测试。** 命令块中的 `sandboxctl`、安装与维护脚本是本项目需要实现的 CLI 契约示例；它们不能被误认为现在已经可运行或已通过。Linux、systemd、curl 等现成命令也只是拟执行示例。所有破坏性故障注入限独立测试机器与可丢弃数据目录，不能在唯一工作区或唯一备份上试验。

上一轮真实 PVE 实验基线为 smolvm v1.37.0。第一阶段实施固定官方 **v1.37.1**，commit `9d3ab4c82756456565544558ac2ea5412afa84ef`，发行时间 `2026-11-24T20:03:13Z`。旧结果可证明已测版本/场景，不自动证明 v1.37.1 或新平台的 systemd、Task Manager、Gateway 组合。v1.37.2 当时只有 tag，不作为已发行二进制基线。[官方 Release](https://github.com/smol-machines/smolvm/releases/tag/v1.37.1)、[版本差异](https://github.com/smol-machines/smolvm/compare/v1.37.0...v1.37.1)

状态、字段、API 和幂等规则以 [SPECIFICATION.md](SPECIFICATION.md) 为准；模块设计见 [DESIGN.md](DESIGN.md)，执行顺序见 [IMPLEMENTATION_PLAN.md](IMPLEMENTATION_PLAN.md)。本文对“通过”的定义是有真实请求、观察与结果记录；UI 截图、绿色按钮、构建成功或第三方产品宣传不能替代该证据。

<a id="test-policy"></a>
## 2. 验证策略：围绕产品风险

功能实现与验证约按 70%/30% 安排，比例是精力方向，不要求工时会计。每个模块完成即执行其最有价值验证；最终组合流程复用这些 fixture。不要为第一阶段重新做完整 VMM/内核审计、Named 网络矩阵、所有存储/CPU/平台组合或全部生态调研。

验证分三层：纯平台状态/幂等/授权/请求转发的契约测试；真实官方 runtime 与受管 Guest 模板的窄集成；Web/CLI/Python/TypeScript 的完整产品流程。mock 可用于可控错误注入，但不能证明 Guest 生存、内存恢复、scope 接管或独立复制。

每份记录至少含平台 commit、runtime Release/commit 与资产 digest、模板 digest/Task Manager 协议版本、Host kernel/CPU/KVM、输入配置、operation/machine/runtime 对应关系、时间范围、预期/实际结果和清理范围。只记录复核所需证据，不默认收集 RAM、任务全文、SSH private key、管理 token 或完整敏感 env。少量性能样本列原值与范围，不凭几次请求报告 p99 或节点密度。

<a id="targeted-gates"></a>
## 3. 五组窄集成门

这些门在实现相应能力时运行，不是要求现在部署。失败只阻塞对应能力；必需的基本持久任务若失败，不能把第一阶段标完成，也不能偷偷替换成普通 exec。

| 门 | 实际要验证的行为 | 通过条件 | 失败后的具体处置 |
|---|---|---|---|
| G1 自有 Guest 监督与持久任务 | native exec background:true 引导 runit；断客户端、重启 platformd/serve/taskd；runner 分离；root payload 使用专属 cgroup | 任务独立生存、重连日志与真实退出、幂等不重复 spawn；合作式 root payload 取消整个 cgroup、receipt writer 存活；无法证实终止时 unknown | 修复自有模板/程序；不改原 Agent、不用 nohup/tmux 冒充；恶意 root 破坏不是强隔离承诺 |
| G2 active recovery 的冻结与取消 | 所有 owned task payload cgroup.freeze 已确认；taskd/runner 在外；full-state capture 后，恢复首次执行仍保持冻结；冻结态 cgroup.kill→确认空→interrupted receipt | 旧任务未在归属裁决前运行；默认 cancel_captured 不产生新外部写入；明确批准 resume_captured 才解冻，未重启 argv | 保留 Live Fork、clean 快照与 Clone；active recovery 显示不可用。若基础 cgroup kill 本身失败，回到 G1 修复受管模板，不能缩小 cancel 承诺却不告知 |
| G3 实时分支与独立闭包 | 运行中的 root Task/普通后台进程/SSH/Exec 存在时 Branch，子接管原进程与日志；clean standalone checkpoint→独立新实例；源资源不可访问后恢复 | 父子各自继续、不重跑 argv；任务身份与数据写入隔离；GTM 降级不禁止原生 Fork；父依赖保护保留；独立副本全 RAM/file canary、cold start、再次恢复通过 | 修平台接管或原生 profile 兼容性；不退化为 clean-only Fork、不把 Live Fork 改名 Independent Clone、不手删共享层 |
| G4 公开接口的端口、身份与网络门 | Branch ports:[] remint；restore query 空 ports 继承；created 后 PUT ports[] 真清空；恢复出站限制；identity/route 核实 | 子 Gateway 服务未提前发布（Guest 内进程可以已在运行）；restore 在 Start 前按策略限制端口/出站；Guest/公网不能绕 Gateway 访问控制 API | 修 Adapter 与模板；不伪造 live deny-all，不把 post-start 清端口当首次指令前 fence |
| G5 真实 systemd/cgroup 重启 | 实际 unit 的 restart、SIGKILL、stop timeout；serve 与 VMM 原生 scope 归属；socket 权限恢复；Host reboot 与仅服务重启分开 | platformd 重启不停止 VMM；serve 重启保留受承诺执行并重新接管；旧连接可断但任务不重复；不留下无法归属的正常实例；恢复 UDS 权限仍最小 | 修自有 unit/安装 helper/权限。不能以 shell detached 成功替代，不能给所有服务套 KillMode=process；不确定实例留 recovery_required，不自动 kill |

G3 的独立性验证可以在同一 PVE 节点使用独立 `SMOLVM_DATA_DIR` 和临时官方 serve/CLI，不要求先提供第二台服务器；隔离重点是不能访问源 runtime state/cache、源进程和源实验制品。若同时运行多个 serve，必须使用不同 UDS、state root 与 `--guest-rollout-port`。不得让验证进程落入用户现用数据目录。[官方多 serve 端口参数](https://github.com/smol-machines/smolvm/blob/9d3ab4c82756456565544558ac2ea5412afa84ef/src/cli/serve.rs#L103-L115)

独立实例仍可能需要自己的导入制品直到 runtime 明确完成内部复制；“不依赖源实例”不等于所有制品可立刻删。每个 clone 检查绑定和公开依赖，版本/profile 首次验收再做完整隔离验证，不对每个普通克隆重复摧毁源。

<a id="acceptance-matrix"></a>
## 4. 产品验收矩阵

| ID | 场景 | 必须观察的结果 | 主要工作包 |
|---|---|---|---|
| A01 | 创建受理后响应丢失，客户端以同 key 重试 | 原 operation 与 runtime 名唯一；不同 fingerprint 明确冲突；无第二台机器；候选 generation 预分配后不复用，失败可留空洞 | P1-W01–03 |
| A02 | operation 已 dispatch，runtime 超时，平台重启 | 继续观察原对象；operation locks 与重型并发位置重建；不按 timeout 新建恢复或释放后盲重试 | P1-W03 |
| A03 | 同机 20 个 HTTP/SSH/TCP wake，首 caller 断开 | 只有一个持久 wake operation/一次实际恢复；其它 waiter 可完成；caller cancel 不取消共享后端动作 | P1-W03/08 |
| A04 | idle 最终检查同时出现 task/HTTP/连接活动 | RPC 前可取消 pause；capture 中记录 wake_pending，确认 pause 后 resume；无对撞恢复、无丢失活动 | P1-W09 |
| A05 | 非幂等 HTTP 请求已可能写入 backend 后断开 | forwarding 保持 may_have_started；Gateway/SDK/HTTP transport 不重放；应用计数至多一次 | P1-W08/10 |
| A06 | 未认证/错误证书/跨对象访问私有 HTTP/TCP/SSH | 拒绝发生在 wake 前；不创建操作或服务探测旁路；事件/列表同样授权 | P1-W02/08 |
| A07 | root SSH/SFTP/WS/大上传/TCP half-close，文件路径处理 | 真实协议成功；连接 hold；root 可读写 Guest 系统文件，文件桥拒绝 Host path/跨机器访问；服务失败不假显示 ready | P1-W04/08 |
| A08 | 新 task 响应丢失、taskd 在 spawn 窗口崩溃 | 同 token 不重复；正确识别 runner/receipt，不能证明时 unknown；不能凭 PID 存在推断成功 | P1-W05 |
| A09 | 任务输出洪水、多 reader、日志过期 | byte offsets/ranges 正确；有界读取与磁盘上限；缺口明确；慢 reader 不拖垮任务和宿主 | P1-W05/10 |
| A10 | native exec 输出多客户端、serve 重启 | 一个上游 reader 处理 after 确认语义；bounded fan-out；session 丢失真实报告，不重跑 command | P1-W04 |
| A11 | task 等模型/I/O，CPU 很低 | task hold 阻止正常 idle；UI/metrics 轮询不被当作用户活动；失联/过期 lease 转 suspect 不直接释放 | P1-W05/09 |
| A12 | native pressure pause/kill 或 OOM | UI/事件显示压力原因，任务重新核对/unknown；不把 hold 宣称抗 emergency，不立即反向 Resume 与 guard 争抢 | P1-W03/09/11 |
| A13 | Live Fork 后请求父 pause/delete，包括 stopped child | 保留上游拒绝，依赖面板说明 blocker；不存在平台 force unlink；逻辑历史树与物理依赖不混用 | P1-W07 |
| A14 | root 活跃 Task、未知任务、未登记后台进程、打开 SSH/Exec 的原生分支 | 只按原生限制/授权/容量/生命周期冲突检查，发起 Live Fork 不要求清理，且不存在应用安全点接口、参数或等待机制；父子计数从捕获点各自推进，独立 Task ID/origin、日志/取消隔离，无 argv 重放；GTM 不可用仍可 Fork 并报告降级，不记录 root taint | P1-W04/05/07 |
| A15 | clean Independent Clone 后源资源不可用 | 子独立验证通过，服务/SSH identity 已更新，历史任务只读带 origin，不自动执行源 task | P1-W06/07 |
| A16 | active recovery 默认取消与显式继续 | 默认 frozen kill 后 interrupted；显式 resume_captured 只同 logical machine、旧执行已 fence、任务重新归属后解冻；无 argv 重放 | P1-W05–07 |
| A17 | rollback 成功/失败/未知，含旧 running、stopped/created、pressurePaused | 候选 generation 预分配不复用；running 先 Stop 确认，paused 的 Stop 409/无 PID 不算 fence；显式 export_then_delete_on_paused 完成导出/pin、非 cascade Delete 且旧名 absent 后才启动候选；paused 分支仅保留制品；当前绑定与 stable URL 只成功切换，新 Start unknown 不启动旧机 | P1-W03/07/08 |
| A18 | 导入损坏/缺 base/路径穿越/超限，伪造 clean/frozen 或替换 manifest 后重算 SHA | 本地 capture provenance+hash 或本机可信 Ed25519 signer 才能支撑声明；未知/验签失败置 quarantined，不 Restore、不启动式 verify；原 last-good 保留；closure 校验不冒称恢复演练成功，私钥不随包导出 | P1-W06 |
| A19 | snapshot 删除与 restore/export 同时发生；磁盘临界 | 活动引用/pin 或 deleting 门阻止竞态；只删平台自有制品；新重型动作可解释拒绝；唯一 last-good 不被自动清掉 | P1-W03/06 |
| A20 | Web 刷新、SDK 断线、token 撤销 | 所有页面接真实 API；找到原操作、按日志 cursor 重连；撤销后不新建隧道/流；unknown/recovery_required 可见 | P1-W10 |
| A21 | 实际 systemd restart/SIGKILL；Host reboot | G5 通过；Host reboot 不承诺未保存 RAM；任务历史按证据判 interrupted/unknown，不自动重新提交 | P1-W11 |
| A22 | 平台 DB 恢复、升级/迁移失败、卸载 | 维护隔离；control_epoch 换新，旧请求不能自动重放；按兼容条件回退；卸载默认保留数据与恢复制品 | P1-W02/11 |

### 完整产品检查

必须逐项完成真实 Web 页面：登录与 token/SSH key、实例列表/详情/创建/配置、所有生命周期动作、分支树和依赖、快照历史/导出导入/恢复回滚、真实终端/文件、任务启动/日志/取消、服务端口/域名/SSH、idle/hold、资源与压力、操作进度/错误/恢复入口、模板和系统设置。没有后台能力的按钮不得伪装完成；资源页不显示尚未实现的精确 CoW/高级配额。

CLI 覆盖主要管理与运维，Python 和 TypeScript SDK 各运行一条真实 agent 流程：创建环境→开始长任务→退出客户端→重新连接日志→查询退出→clean checkpoint/clone→访问服务→查询 operation。WebTerminal/SFTP 用真实协议测试；SDK 不只是生成的类型文件，必须有等待、分页/游标、流恢复、幂等与错误处理。

<a id="deployment"></a>
## 5. 安装与部署运行手册

### 5.1 进程、账户与目录

目标进程是一个 `platformd` 和一个官方 `smolvm serve`。UI/Gateway/任务 bridge/生命周期/SQLite 都在 platformd 内；无常驻 Node、独立 Gateway、CoreDNS、Host SSH bastion、Redis 或队列服务。Guest 内新增 runit 与自有任务程序、OpenSSH，不修改内置 Guest Agent。

| 对象 | 默认位置/身份 | 权限与生命周期 |
|---|---|---|
| 平台配置 | `/etc/sandbox-platform/config.yaml` | 管理员写；secret 独立受控文件，日志不打印 |
| 平台数据 | `/var/lib/sandbox-platform/` | 非 root `sandbox-platform` 拥有；DB、catalog、独立制品、逻辑 SSH 身份 |
| runtime state root | `/var/lib/smolvm-platform-runtime/`，`SMOLVM_DATA_DIR` 指定 | runtime 私有；平台不读写内部 DB/控制协议/层文件 |
| runtime HTTP UDS | `/run/smolvm-platform/api.sock` | 不暴露网络、不挂 Guest；仅 runtime 与 platform 控制组可访问 |
| platformd unit | `platformd.service`，非 root `sandbox-platform` | 停止只断管理/转发连接，不停止 VM |
| serve unit | `smolvm-serve.service`，官方原生 scopes 所需的特权身份 | runtime 是宿主高权限边界；受私有 UDS、文件权限与既有隔离约束 |
| Guest 数据 | `/workspace`、`/var/lib/sandbox-taskd` | 内部持久磁盘；root payload 与 manager/runner 位于不同 cgroup |

**原生 scope 的实际前提不能省略。** 固定源码 `systemd_scope::is_available()` 同时要求 root、systemd 和 busctl；非 root serve 回退到 direct cgroup，源码明确不提供同样无损的 service restart 模式。因此首期默认 platformd 非 root、官方 serve 使用支持原生 scopes 的 root 身份，并验证其原有 VMM UID/seccomp/Landlock；不能声称两个非 root 服务已天然获得原生独立 scope。若以后选择其它 supervisor/cgroup 方案，应单独验证后再改部署契约。[scope 可用条件](https://github.com/smol-machines/smolvm/blob/9d3ab4c82756456565544558ac2ea5412afa84ef/src/systemd_scope.rs#L421-L450)

用户已确认允许 root；完整 runtime API 访问本身是宿主特权边界，platformd 虽非 root 也不是与宿主强隔离的普通低权限应用。需要补口的公开 CLI 只能由固定 root-owned allowlist wrapper/oneshot 执行，禁止任意 sudo shell、动态子命令和用户 Host 路径。

serve 在新 socket 上显式设置 0600 后 rename 发布；仅设置组或 umask 不会自动允许另一个平台 UID。安装需要自有受控 post-start helper：等待正确 path 的 socket，核验类型、owner、父目录与当前服务，在平台开始调用前授予唯一 `sandbox-control` 组 0660（或等价专用 ACL）；每次 serve 重建都执行。初始仍为 0600，不存在先对全用户开放的窗口。父目录 `/run/smolvm-platform` 为 root:sandbox-control 0770，组成员仅平台服务账户，socket 不允许 Guest 或普通登录用户读写；helper 不接受远程指定 Host 路径。[socket 创建](https://github.com/smol-machines/smolvm/blob/9d3ab4c82756456565544558ac2ea5412afa84ef/src/cli/serve.rs#L1047-L1074)

Guest 默认 root（UID/GID 0）。SSH 公钥认证、关闭密码；文件 API 允许目标 Guest 系统路径，复用 native files，按需使用固定无状态 helper 和 staging。宿主路径、Host mount、runtime UDS、宿主管理网与跨机器访问仍受原安全边界保护，不把 Guest root 等同宿主 root。

正常 root 操作不触发永久失效，也不禁止原生 Live Fork。GTM 或其记录被修改、失联、无法核实时，基于实时探针显示 `guest_management_status=degraded/unknown`，只限制依赖该证据的任务查询、受控快照和自动 idle；恢复健康后更新。取消/冻结无法核实不得宣称成功。

### 5.2 安装前检查

检查 L1 Linux 的 `/dev/kvm`、CPU 虚拟化暴露、架构、磁盘 free/inodes、systemd/busctl、cgroup v2、TLS/DNS 与可用端口；记录 PVE/L1 版本和存储位置。PVE nested KVM 用于让 L1 跑 L2，不要求向每个用户 Guest 开启额外 `nestedVirt:true` 或 `--allow-nested-virt`；这些会扩大不同的边界。

```bash
# 以下均为实施阶段拟执行的只读预检，本轮未执行。
uname -a
test -c /dev/kvm
test -d /sys/fs/cgroup
stat -fc %T /sys/fs/cgroup
command -v busctl
systemctl --version
df -h /var/lib
df -i /var/lib
free -m
ss -ltn
```

安装器先验证发行 manifest、v1.37.1 官方 Linux 资产与配套依赖、checksum、模板 digest，再创建账户/目录/unit。checksum 校验内容完整性，不应被描述为官方 archive 签名；官方资产和模板来源须固定。用户已有同名 data root/机器时先检测冲突，不覆盖或清空其数据。

```bash
# 拟实现安装脚本的调用契约；在 P1-W11 完成前不可运行。
sudo ./packaging/install/install.sh \
  --manifest ./release-manifest.json \
  --config /etc/sandbox-platform/config.yaml

# 拟实现 CLI；检查生效配置、版本、权限、网络与 Guest 能力。
sandboxctl doctor --json
```

runtime 的受控启动形式为 `smolvm serve start --listen unix:///run/smolvm-platform/api.sock`。unit 显式设置 `SMOLVM_DATA_DIR=/var/lib/smolvm-platform-runtime`、`SMOLVM_PUBLISH_ADDR=127.0.0.1`、`SMOLVM_EGRESS_FLOOR=strict`，保留 native pressure guard；普通机器显式 virtio-net。环境配置不向用户暴露为任意 runtime 参数透传。资产实际 binary 路径由 manifest/installer 决定，不依赖用户交互 shell 的 PATH。

### 5.3 网络与入口安全验收

1. 完整 runtime API 只在 UDS；从 Guest、LAN 和公网都不能通过 HTTP 调到 machine/file/exec 管理操作。原生 rollout ingress 是另一个带 lease 认证的受限入口，要单独确认未授权拒绝，不能误称“Guest 与 Host 没有任何通信”。
2. 原生 ports 仅 Host loopback；Host 防火墙不开放其高端口。strict egress 阻 private/loopback/link-local/CGNAT 等固定范围，**不会自动枚举 Host 公共 IPv4/IPv6**；管理公网地址另用已有 deny CIDR/Host 防火墙保护，覆盖 IPv4-mapped IPv6。不能只靠 loopback bind 阻 Guest 旁路。
3. 管理 origin 与 Guest 服务 origin 分开；管理 cookie 为 host-only，不设置 wildcard Domain。HTTP Host/SNI 与服务登记匹配。未登记 Host 拒绝，不能让任意 Guest 内容进入管理 origin。
4. 默认 TLS 使用用户提供证书；需要自动证书时可选 Caddy。若 TLS 在 Caddy 终止，平台只信任受控链路的 Forwarded/身份信息，不信任公网同名头。通用反代必须关闭会重复业务请求的自动重试。
5. HTTPS/WebSocket tunnel 先鉴权再 wake。浏览器使用短期单用 stream ticket，不把长期 bearer 放 URL；CLI WebSocket 用 auth header。SSH 内层继续验 Guest key/host key，不关闭 known_hosts 验证。
6. 外部固定 TCP port 必须 mTLS 成功后再 wake；不能原生 mTLS 的普通客户端使用 `sandboxctl port-forward`。源 CIDR 可额外限制，但不替代用户身份。每个 TCP listener 显式归属 service；DNS 名不同不产生同 IP/port 的自动 TCP 分流。

来源：[loopback publish](https://github.com/smol-machines/smolvm/blob/9d3ab4c82756456565544558ac2ea5412afa84ef/crates/smolvm-network/src/publish_bind.rs#L1-L126)、[strict 与本地默认](https://github.com/smol-machines/smolvm/blob/9d3ab4c82756456565544558ac2ea5412afa84ef/crates/smolvm-network/src/egress.rs#L267-L350)、[固定地址范围](https://github.com/smol-machines/smolvm/blob/9d3ab4c82756456565544558ac2ea5412afa84ef/crates/smolvm-network/src/egress.rs#L390-L431)、[Guest rollout listener](https://github.com/smol-machines/smolvm/blob/9d3ab4c82756456565544558ac2ea5412afa84ef/src/cli/serve.rs#L608-L635)

### 5.4 真实服务重启门

启动顺序是 runtime 健康→UDS 授权完成→platform 单写者锁与 reconcile→管理/服务正常接入。platformd 启动后先显示协调状态，不在未决操作未核对前自动 idle/GC/重复派发。平台停止应停止接收新请求、保存自身状态和断流，不能把“停止管理服务”解释为“关掉所有用户 VM”。

在可丢弃 fixture 中记录 task/runtime identity 与实际 cgroup 后执行：

```bash
# 实施阶段必须执行的 systemd 集成示例；本轮未执行。
systemctl show platformd.service -p User -p ControlGroup -p KillMode
systemctl show smolvm-serve.service -p User -p ControlGroup -p KillMode
systemctl list-units --type=scope 'smolvm-vm-*'
sudo systemctl restart platformd.service
sandboxctl doctor --json
sudo systemctl restart smolvm-serve.service
sandboxctl doctor --json
```

然后定向执行 SIGKILL、stop timeout 与重启后的原操作查询；记录旧 SSH/WS 断开允许，持久任务是否仍有真实 runner/日志/退出。不要把 `KillMode=process` 全局写进 unit 当答案；它可能留下未管理后代。检查运行 VMM 是否确实处于独立原生 scope，不能只有 unit 配置文字。Host reboot 单列：未保存 RAM 消失属于另一故障域，平台只能从明确恢复点和持久磁盘恢复。[systemd.kill](https://www.freedesktop.org/software/systemd/man/latest/systemd.kill.html)

<a id="daily-operations"></a>
## 6. 日常操作、资源与保活

### 6.1 日常观察

UI 与 `sandboxctl` 显示 machine 的 observed_state/availability、当前 runtime identity、binding_generation、active operation、最后观测时间、任务 hold、连接 hold、suspect lease、native stateReason。`running` 不代表每个应用 ready；每个 service 有当前 backend 的独立 readiness。

```bash
# 拟实现 CLI 示例；标识符由真实列表返回，示例值不可直接照抄。
sandboxctl machines list
sandboxctl machines get MACHINE_ID
sandboxctl operations get OPERATION_ID
sandboxctl operations reconcile OPERATION_ID
sandboxctl tasks get TASK_ID
sandboxctl tasks logs TASK_ID --follow
```

`operations cancel` 只允许尚未 dispatch 的操作取消；断开等待不撤销已发生的 runtime 效果。`reconcile` 使用原名称/收据重新观察，不能换名创建替代 VM。任务 cancel 则是单独的受理动作：确认目标 task_namespace/runner/cgroup 后终止 payload tree，保留 receipt writer；失败或未知不伪装 cancelled。

### 6.2 默认资源与 idle 政策

| 设置 | 初值/边界 | 运维含义 |
|---|---|---|
| 全节点重型操作并发 | 1 | 覆盖重型 start/resume/import/checkpoint/clone 子计划；复合操作复用同一许可；未决操作重启后继续占排程位置 |
| Host 空间临界 | 初始 `max(2 GiB, 文件系统容量×15%)`，可配置 | 不保证最坏 capture 峰值足够；只作简单最低余量门，新增长动作临界拒绝，原生错误仍如实返回 |
| 托管 idle | G1/G4/G5 等相关门通过后默认 30 分钟 | 任务/连接/显式 keep_running 会阻止正常 idle；custom 模板默认关闭自动 idle |
| user keep_running | 用户显式启用持续有效 | 展示责任主体；不能因 CPU 低被自动撤销 |
| task/lease 状态陈旧 | suspect | 不直接释放 hold；恢复查询或管理员明确处理 |
| 原生 pressure guard | 保留 | 平台 hold 只约束正常 idle；紧急 pause/kill、内核 OOM 仍可能发生 |
| 预热池/高级预测 | 不启用 | 不为长期 workspace 引入 pool expiry、CoW 预测和担保容量 |

阈值是首个节点的起点，不是容量测量结论。展示已有 Host MemAvailable/pressure、cgroup 事件、VM 配置与 native metrics、disk free/inode、FD/连接、日志大小与排队原因；不把多 VM RSS 相加当真实独占内存，也不显示不存在的 reservation/unaccounted 账。原生 CPU/内存/磁盘配置与 admission 直接复用，平台不重造 OOM 受害者算法。

pressure guard 可能暂停 child，严重 slice 压力下还可 kill 非 source/非 held pool VM；它不认识平台 task hold。出现此类事件，先降低新增工作并核对压力/任务状态，不立即反复 Resume 与 guard 竞争。不能为了让 hold 看起来“绝对有效”关闭所有保护。[压力机制](https://github.com/smol-machines/smolvm/blob/9d3ab4c82756456565544558ac2ea5412afa84ef/src/api/memory_pressure.rs#L1-L54)、[候选字段](https://github.com/smol-machines/smolvm/blob/9d3ab4c82756456565544558ac2ea5412afa84ef/src/api/memory_pressure.rs#L187-L225)

低空间处置顺序：拒绝新 capture/import/restore 等增长动作→阻止继续扩大日志/临时导入→清理明确失败且无引用的自有暂存、按既定日志保留轮转→按用户 retention 删除可删独立备份→扩容。不得删唯一 last-good、paused 继续所需 artifact、active operation pin 或 runtime 私有层。删除文件名不证明 Host 物理空间已回收，实际空间重新测量。

<a id="backup-restore"></a>
## 7. 备份、恢复、回滚与任务处置

### 7.1 备份内容

| 类别 | 包含 | 不能推出的结论 |
|---|---|---|
| runtime pause saved execution | 单次继续执行所需状态，原 runtime_pause_id | Resume 消费后仍是长期备份 |
| 平台 snapshot/export | 明确 capture_scope、encoding、制品与全部 base/pack 依赖、不可变版本/模板/任务 manifest 及 manifest.sig | 任意 CPU/版本可恢复；已撤销外部副作用 |
| 平台 DB/config 备份 | SQLite 一致副本、模板与 service/catalog 配置、control 历史、capture provenance 与可信 signer 配置；私钥另行加密备份 | 单独恢复未保存 RAM；自动知道 DB 备份之后所有请求是否已执行 |
| 不同故障域副本 | 完整包和必要解密材料能在源节点不可用时取回 | 同 Host 另一目录或同 PVE 存储池天然是灾备 |

默认 standalone/full-state；history 是平台快照列表。runtime 内部增量/CoW 继续由 runtime 管理，可选增量导出只有公开闭包验证后才启用。`closure_verified_at` 表示内容/依赖校验，`restore_verified_at` 表示实际恢复验证，两者不能合并。last-good 必须指独立已提交制品，不能指被 Resume 消费的 pause artifact。

SHA256 可证明字节与给定摘要一致，不能证明导入者写的 `task_content=clean`、frozen 或 guest_management_status 声明真实。自有 capture 的来源、operation、绑定、屏障/冻结结果与工件 hash 必须耐久关联。导出使用已固定的 `manifest.json` 原始字节及 Ed25519 `manifest.sig`，签名覆盖其中的 runtime 文件 hash、长度、task_content、guest_management_status、barrier/freeze 与来源信息；验签不能先重新序列化 JSON，后续验证时间更新留在 catalog，不悄悄改已签字节。导入只接受与本地 capture 记录精确匹配的内容，或由本机预先配置可信公钥验证通过的 manifest；manifest 自带的公钥/key ID 不会自动被信任。

签名私钥保存在宿主受保护 secret store，不进入 Guest、普通 snapshot export 或诊断包；本机备份另外加密保存它及恢复所需材料，可信 signer 公钥/标识属于本地受控配置。验签表明可信来源为这些声明背书，仍不替代 profile、版本、冻结和兼容检查。未知来源即便所有 SHA 正确且自称 clean/frozen，也置 quarantined，允许静态取证，禁止自动 Restore 与会启动 Guest 的在线 verify；不能用“先启动看看”建立信任。

### 7.2 创建基本备份

1. 查询当前 operation、task 与服务，选择 purpose=clone 的合作式 clean 恢复点或已通过 G2 的 recovery 恢复点，按各自捕获策略处理。Live Fork 单独走原生实时分支，不使用该备份屏障。
2. 通过公开 runtime checkpoint/export 生成制品，完成后验证摘要/manifest/依赖闭包并提交 capture provenance，再签不可变 manifest；失败暂存不成为 ready。结果 unknown 保留原操作与屏障，先观察，不能重发 capture 并任意解冻。
3. 使用 SQLite 在线备份接口或受控停写/检查点流程生成一致副本；运行中不能只复制主 DB 而忽略 WAL。记录 DB 备份时刻与制品清单；跨运行时/平台/文件系统不存在自动全局事务，恢复仍需 reconcile。[SQLite backup](https://www.sqlite.org/backup.html)、[WAL](https://www.sqlite.org/wal.html)
4. 打包平台配置、版本 manifest、已准备好的独立制品、manifest.sig 与来源记录；私钥只进入另行加密的本机管理员备份，不混入普通快照导出包。只导出明确管理的数据，不靠读取 runtime 私有 DB 重建产品状态。
5. 拷到管理员配置的目标目录/存储，验证可读与 checksum。目标实际故障域由运维确认；需要节点灾备时应离开该节点和同一故障存储。首期不强制 S3 服务或新存储引擎。

```bash
# 拟实现的本机管理员命令；目标路径仅本机授权使用，不是远程任意 Host path API。
sudo sandboxctl admin backup create \
  --output /mnt/sandbox-backup/backup-2026-11-25
sudo sandboxctl admin backup verify \
  --archive /mnt/sandbox-backup/backup-2026-11-25
```

`backup verify` 的默认含义是静态来源/签名、闭包与摘要验证；执行恢复演练还须来源可信、策略合格、显式选择且具备隔离目标，不能为验证未知签名而先启动，也不能默默启动第二份生产写者。备份 RAM/Guest 磁盘中可能有令牌/SSH key，导出下载、目标目录、解密密钥、审计与保留均按敏感数据处理。

### 7.3 新实例导入与独立恢复

先校验本地 capture provenance 或可信 Ed25519 signer，再验证不可变 manifest、runtime 文件 digest、大小、路径/解压边界、snapshot 格式及 CPU/架构/runtime/模板兼容。未知或验签失败的包停在 quarantined，不调用 runtime Start/在线 verify。新 logical machine 只从来源可信且 `task_content=clean` 的制品恢复；active/unknown 制品不能借自报 clean 或新机 restore 绕过任务继承限制。

按公开顺序导入到全新 runtime 名并保持 created；restore query `ports=[]` 仍会继承源端口，必须在 Start 前公开 `PUT /api/v1/machines/{name}/ports {"ports":[]}` 后 GET 确认。使用公开 import `allowed_cidrs=[]`/`allowed_hosts=[]` 与受支持 basic allowlist profile，使首次执行没有外发能力；源 egressRules 必须满足规范的空/basic 条件。不要添加无法 live 移除的通用 deny rule，也不要把 live network 空列表理解为断开全部现有 TCP。[restore 查询](https://github.com/smol-machines/smolvm/blob/9d3ab4c82756456565544558ac2ea5412afa84ef/src/api/handlers/machines.rs#L1692-L1725)、[created 导入](https://github.com/smol-machines/smolvm/blob/9d3ab4c82756456565544558ac2ea5412afa84ef/src/api/handlers/machines.rs#L3569-L3604)、[端口继承](https://github.com/smol-machines/smolvm/blob/9d3ab4c82756456565544558ac2ea5412afa84ef/src/machine_spec.rs#L1440-L1478)

Start 后仍保持复制屏障，确认 manager、task namespace、历史 origin、SSH 身份和服务配置。非空 allowCidrs 在 strict floor 下恢复受支持出站；只在 ready 后发布 Gateway 服务。失败保留来源与原操作；结果 unknown 不换名再恢复。独立性验证通过后才发布该版本/profile 的 Independent Clone 能力，不能仅因机器看起来 running 就宣称无源依赖。

### 7.4 同一机器 rollback 与 active recovery

1. 核对目标恢复点的可信 provenance/签名、task_content/guest_management_status、任务/freeze manifest 与兼容性；向用户展示将失去的新状态、不能回滚的外部效果及任务处置。默认先保存当前恢复点。**若旧对象是 paused saved，必须显式采用 `old_state_policy=export_then_delete_on_paused`，当前 pause 工件的成功导出与 pin 是必需条件，不能选择跳过后直接删除。** UI/CLI 同时说明此分支会删除旧 runtime，失败回退只保留制品，不再有可直接 Start 的旧对象。
2. 关闭新入口/任务/exec，在机器操作锁下读取新鲜状态并按下表建立 fence。一个 generation 更新、没有 PID 或 `paused` 标志都不是充分证明。尤其 native pressure pause 可能由 guard 自行 Resume；不能因当时无 PID 就并行启动候选。

| 旧 runtime 的新鲜状态 | 必须先完成的动作 | 允许启动候选的条件 |
|---|---|---|
| running | 经原生 Stop 停止并确认 stopped 和有效 restart.policy=never；保留旧 runtime 也不自动重启它 | 对象身份连续、没有 saved execution/未决启动或恢复、确认已停；期间出现 pressure pause 转下一分支或阻塞 |
| created/stopped | 核对当前对象身份、有效 restart.policy=never，没有 saved execution、未决 Start/Resume/自动重启意图和其它控制方 | 证据新鲜且状态确定；不能只看到一个空 PID |
| paused saved，含 pressurePaused | 公开 `GET /pause/checkpoint` 导出当前保存态（已知原 pause ID 时传原 ID），完整校验、写 catalog 并 pin；随后原生 `DELETE ...?cascade=false` 删除旧 runtime | 独立导出已受保护，Delete 成功且公开详情确认旧名字 absent；不得对 paused 发 Stop 后把 409 当完成 |
| unknown/状态跳变/导出失败/依赖拒绝/Delete 结果不明 | 保留锁、pins、原意图与诊断，继续 reconcile | 不 Start 候选，不强制 cascade、不手删 runtime 数据 |

3. 候选 generation 在首次 RPC 前按全历史 MAX+1 耐久分配，失败不复用、允许空洞。只有上表 fence 完成后才可 Start；导入的新对象先保持 created，公开清 ports/限制出站并确认。含 active task 的目标还必须在首次执行保留已验证 freezer 约束。
4. 默认 `task_disposition=cancel_captured`：保持冻结时 cgroup.kill，确认 payload 为空，写 interrupted 收据。明确选择 `resume_captured` 仅限同 logical machine、旧执行已按表 fence、来源可信的匹配任务 manifest 且用户接受外部副作用回退风险；重新绑定 namespace/origin 后才解冻原保存进程，不能重新 exec argv。
5. 新 runtime/任务/服务完成检查后，提交预分配的 binding_generation 为当前、清旧 route cache、发布相同 machine/service URL。普通 Pause/Resume 不增加此 generation。
6. 新 Start 结果 unknown 时保留恢复状态、锁和重型位置继续观察，不能启动旧机补偿。running→stopped 分支若以后要另行回退，必须先确认候选已经停止再明确操作；paused→Delete 分支旧 runtime 已不存在，只能按同样的信任/任务/恢复门另行使用被 pin 的导出制品。确认当前稳定后才按 retention 清理。

导出的旧 pause 工件如缺少可信的 clean/frozen 证明，应按实际 task_content/捕获证据 保留为受限恢复或证据，不能因导出成功就宣称可自动执行回退。源 state 在导出/删除期间变化或结果未知时重新协调，不把此前一次 paused 观察永久当作 fence。[原生 Stop 的 saved-execution 拒绝](https://github.com/smol-machines/smolvm/blob/9d3ab4c82756456565544558ac2ea5412afa84ef/src/api/handlers/machines.rs#L6180-L6225)、[pressure guard 自动 Resume](https://github.com/smol-machines/smolvm/blob/9d3ab4c82756456565544558ac2ea5412afa84ef/src/api/memory_pressure.rs#L1-L19)、[Delete 依赖保护](https://github.com/smol-machines/smolvm/blob/9d3ab4c82756456565544558ac2ea5412afa84ef/src/api/handlers/machines.rs#L6576-L6649)

快照会回退文件、进程和凭据，却无法撤销已经发送的邮件、写入的远端数据库或调用的外部 API。即使只有一个新实例，历史任务继续执行仍可能重复外部动作，因此默认取消 captured tasks，并记录用户显式选择继续时的审计。无法验证 freeze/任务 manifest 的制品保持 unknown，不能作为 clean 绕过。

### 7.5 平台数据库备份恢复

DB 恢复走本机管理员维护命令，不能直接覆盖正在使用的 SQLite 文件：关闭 mutation、停止旧 dispatch owner、持单写者锁，保存当前可回退副本；恢复 DB 后在开放入口前换新 `control_epoch`，记录备份来源与历史缺口，进入 reconcile。普通进程重启保持原 epoch。

```bash
# 拟实现的维护流程示例；不是已经存在或本轮执行过的命令。
sudo sandboxctl admin maintenance enter --reason database-restore
sudo sandboxctl admin backup restore \
  --archive /mnt/sandbox-backup/backup-2026-11-25
sandboxctl doctor --json
sudo sandboxctl admin maintenance exit
```

备份中 accepted/queued 或缺失幂等 key 不能证明相应请求未执行。旧 `If-Control-Epoch` 拒绝；SDK 不刷新 epoch 后自动补发原请求，也不换 key 重放。先查询现有 runtime、任务 journal、catalog 与服务，再逐项解除安全范围的维护门。对已丢失收据的副作用保留 unknown。安装标记和受控恢复流程能处理本平台工具的恢复；不能宣称自动发现任意外部 PVE 整盘回滚。

<a id="upgrade"></a>
## 8. 升级、回退与卸载

升级对象是官方 runtime 发行物、platformd/CLI/SDK、托管模板与自有 Task Manager。固定运行时资产和模板 digest；不维护私有底座补丁队列。新 upstream release 先分类安全/数据/行为变化，仅回归受影响路径与五组门；不可把移动 latest 自动替换为生产基线。[官方安全支持政策](https://github.com/smol-machines/smolvm/blob/9d3ab4c82756456565544558ac2ea5412afa84ef/SECURITY.md)

升级前进入维护模式，停止新增重型操作，确认未决操作/任务状态，生成 DB/config/独立恢复点备份；记录旧 binary/依赖/Guest 资产，确保可以获得。DB 迁移向前执行并保留恢复办法。已有 running VM 的内存、Guest Agent/kernel、snapshot 不因 Host binary 更换自动升级；检查旧执行接管与旧制品恢复兼容后再开放新动作。

```bash
# 拟实现的本机升级命令；plan 只读，apply 仅在备份/兼容门通过后执行。
sudo sandboxctl admin upgrade plan --manifest ./release-manifest.json
sudo sandboxctl admin maintenance enter --reason release-upgrade
sudo sandboxctl admin upgrade apply --manifest ./release-manifest.json
sandboxctl doctor --json
sudo sandboxctl admin maintenance exit
```

回退先区分仅替换 binary、平台 schema 回退、runtime/snapshot 格式回退。未经确认不把新 DB/新制品交旧版本；失败保留当前数据及诊断，进入 recovery_required/维护状态。旧 snapshot 的恢复资产要保留到新版本实际演练成功，不因升级“看起来正常”立即清空。

卸载默认停止 platformd/入口并移除自有 unit/binary，保留平台 DB、config/keys、备份和 runtime 用户数据。停用 runtime 前展示仍运行/paused/有子依赖的机器及明确选择，不悄悄删机器；销毁数据为单独显式本机操作，按范围列清单，不提供通用 `rm -rf` 快捷步骤。不移除用户原有独立 smolvm 安装或其它服务。

<a id="troubleshooting"></a>
## 9. 故障诊断与人工恢复

先保留 operation_id、machine_id、binding_generation、runtime_name/runtime identity、control_epoch、版本、最后观测时间、stateReason 与相关 task namespace。未知不等于不存在；不要先清 DB 或目录“重置”。

| 症状 | 核对证据 | 处置 | 不允许的捷径 |
|---|---|---|---|
| Create/Restore 响应超时 | 原 operation、唯一 runtime 名、公开 identity/状态 | reconcile 原操作，保留重型位置 | 换名再创建、timeout 就释放 |
| 平台重启后任务显示 unknown | Guest journal、runner/boot/PID/start/cgroup、final receipt | 原任务查询、保留 hold、人工裁决必要时取消确认目标 | 自动重新 exec，凭 PID 活着判成功 |
| serve 重启 native exec ID 404 | 原 session、生存模式、输出游标 | 报会话丢失/未知；持久任务走 Task Manager 查询 | 把 session 404 当任务结束或自动重跑 |
| 父 Pause/Delete 被拒绝 | parent/child 原生依赖，含 stopped child | 显示 blocker，从明确子对象处理或另建独立副本 | force/unlink runtime golden/backing |
| running 但 HTTP/SSH 不通 | 当前 runtime identity、native ports、service readiness、Gateway cache | 失效旧路由，检查 Guest listener/sshd | 再次 Resume，复用旧 Host port |
| 新副本 Gateway 提前发布 | Branch ports[] remint、子 identity、route readiness；独立 Clone 的捕获协调 | 撤销提前发布的路由、保留证据，修发布序列 | 把 Live Fork 中子进程已执行视为错误，或宣称 post-start 清 ports 阻止外联 |
| Guest 能连 Host 控制/服务 | strict 生效值、virtio-net、公开 Host IP 策略、UDS/端口 | 恢复最小开放边界并逐路径复测 | 只换一个 loopback 端口，关闭所有隔离来调试 |
| native pressure pause/kill，paused rollback Stop 409 | stateReason、saved execution、原生 guard、导出/pin/Delete 状态 | 普通情况先查压力；回滚按显式 export_then_delete_on_paused，旧名 absent 才 Start 候选 | 当 idle；无 PID 即认为 fence；Stop 409 后双开；盲目关闭 guard |
| SSH host key 变化 | logical machine 与 copy/rollback 记录、可信指纹 | 按独立身份流程重新绑定/明确更新 known_hosts | StrictHostKeyChecking=no、给所有子复用源 key |
| 备份离源不可恢复或导入自称 clean | provenance/可信 signer、不可变 manifest 与签名、base/pack/格式/CPU、源 cache | 隔离该代，保留前代与来源，修闭包或信任配置 | SHA 通过就信 clean/frozen，先启动未知工件验真，拼旧 RAM 和新磁盘 |
| DB 备份恢复后请求不明 | 新 control_epoch、旧操作收据、runtime/task 真状态 | 维护隔离、重观察、保留历史缺口 | 刷新 epoch 后自动补发 pending |
| UDS 重启后 Permission denied | 新 socket owner/mode、父目录、helper 状态 | 修受控 post-start 授权，保持私有边界 | chmod777 或暴露完整 HTTP 控制 API |
| 删除成功但磁盘没释放 | 实际 allocated/free、打开引用、平台 pin/依赖 | 按真实 free 评估，避免继续扩大占用 | 手删 runtime 私有层，按文件名数量估容量 |

诊断命令输出 JSON/包：版本与有效非敏感配置、operation/history、runtime 公开观察、task 元数据、route/port 映射、scope/cgroup、pressure、disk/inode、日志摘要。默认脱敏命令 secret/env、URL query、token、SSH key 和任务正文；需要更详细 Guest 内容由用户显式选择。受权诊断包不得包含可直接调用 runtime 的私有 endpoint 凭据。

```bash
# 拟实现命令；输出路径在本机授权范围，默认脱敏。
sandboxctl diagnostics export --output ./sandbox-diagnostics.zip
sudo journalctl -u platformd.service --since '30 minutes ago'
sudo journalctl -u smolvm-serve.service --since '30 minutes ago'
```

人工恢复入口应提供“继续观察、确认停止原执行后恢复、取消特定 task、核对并认领明确对象、放弃指定数据”等具体动作，展示作用对象与风险，仍记录 operation/审计。不得提供一键“清理所有异常”绕过身份和引用保护。

<a id="release-checklist"></a>
## 10. 第一阶段放行标准

第一阶段完成需要：11 个工作包交付实际结果；全部要求的 UI/CLI/SDK 流程可用；五组窄门有真实记录或对非必需增强路径有明确受限结果；A01–A22 相关产品风险有验证；干净安装、真实服务重启、备份与升级/回退/卸载流程完成演练；已知限制在 UI/API/运行手册中一致。

允许明确受限的是未通过特定组合门的 profile、active task snapshot 恢复等增强路径；不允许把核心持久任务、Independent Clone、普通网络访问、完整客户端或基本运维缺失包装为“第一阶段完成”。若核心能力受官方公开接口所限，报告固定版本证据与准确 blocker，继续完成其余已授权工作，等待能力或范围的明确决策；不能为按时交付私改底座。

首期不包括 Named/Guest 稳定私网 IP、外部卷、就地 detach、高级资源账、预测/OOM 策略、多节点、Mac、E2B 兼容或企业计费。运维手册不应出现这些功能的常规操作菜单，避免给出它们已受支持的印象。
