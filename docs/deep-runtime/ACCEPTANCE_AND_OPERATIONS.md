# 验收与运维

本文规定 deep-runtime 路线在 Linux x86_64 Worker 上的验收和运行流程，覆盖 PVE 嵌套 KVM，单节点优先。

2026-11-26，隔离 Worker 已完成候选安装、真实 KVM 故障恢复和同机性能测量。核对一次部署时，从验收台账定位所用 Bundle、Guest profile、实测结果和原始日志。各次发布保留独立摘要。[验收台账](acceptance-ledger.md) · [性能原始数据索引](performance-wp22.md) · [整机恢复记录](managed-recovery.md#worker-主机换-boot-后的恢复)

## 1. 验证原则与分层

主要精力投入实现，只验证改动带来的真实风险。所有验证保留 source SHA、Bundle digest、host kernel/CPU/KVM、PVE层级、Guest profile、存储栈、网络模式、运行参数和原始结果。未变的Phase 1认证、终端、文件等只跑必要兼容冒烟；不得每包重新做一遍基础评测。

| 级别 | 触发 | 内容 |
| --- | --- | --- |
| L0：契约/静态 | schema、协议、构建、SQL改动 | digest/版本/边界校验、迁移fixture、接口/类型构建、错误映射 |
| L1：窄功能 | 一个具体runtime/产品能力变化 | 受影响profile的一条真实功能路径和失败路径 |
| L2：状态正确性 | 内存、设备、磁盘、gate、fencing、serializer变化 | 必须的冷页/多盘/身份/commit故障注入和旧状态reader |
| L3：性能/密度 | 新瓶颈优化、资源预算或默认参数变化 | 同机对照的阶段耗时、首任务、总吞吐、内存/线程/I/O；不是常态全矩阵 |
| L4：发布 | 核心能力组合首次上线、新Bundle成为默认 | 代表性全流程、旧Bundle恢复、服务重启、部署隔离与备份恢复 |

协议schema/测试fixture由接口拥有者维护，其他仓库复用相同golden数据。测试不能只assert实现内部变量；必须从CPU/RAM/磁盘/网络/资源残留的外部行为判断承诺是否成立。

## 2. 核心能力完成矩阵

| ID | 场景与方法 | 通过标准 / 失败边界 | 触发工作包 |
| --- | --- | --- | --- |
| A01 | native Fork前后追踪文件/复制量；批量子比较capture_id和backing refs | 不以完整RAM持久化/全拷贝作为必经步骤；同批同capture | 05/06/12 |
| A02 | 父写入多代数据、子部分RAM页不预触碰；删父和祖先后再访问冷页/盘 | 内容/FD语义正确，无父目录/socket依赖；子可继续Fork | 05/12 |
| A03 | 递归树、删除不同祖先、每个分支写私有页和盘，子独立Hibernate/Restore | 父/兄弟无串写；逻辑谱系保留但不阻止资源安全释放 | 05/12/13 |
| A04 | broker kill/restart；分别有活VMMprovider、仅broker持有、同时杀唯一provider | 可提供FD的资源恢复；无provider volatile明确lost；不补零、不错误GC | 04/05/23 |
| A05 | 最后引用、未决operation、backup/export pin、hibernate root同时竞争GC | 只有不可达且无消费者对象回收；tombstone/重启可继续；无悬挂引用 | 04/13/19 |
| A06 | capture每阶段断连与崩溃；source执行并发变化 | Query返回真实decision；不重复capture同op；FAILED_HELD不擅自Resume | 06/13 |
| A07 | Hibernate捕获前后记录Guest计数器、磁盘日志和外部受控观察；无后代/有活后代分别持久化回收 | capture后到exit无用户/内核继续执行窗口；STOP前失败回原态，STOP后只完成exit；durable根可独立恢复，冗余volatile pins释放而后代冷页仍可读，报告实际回收量 | 13 |
| A08 | managed Standby、raw Standby、长待机后capture/Hibernate、Wake | 无RAM落盘/新generation；managed待机原点不被偷偷推进；raw strictclone拒绝；时钟不重复/遗漏调整 | 07/13 |
| A09 | 两盘+RAM记录状态机phase，持续交错写入并在任意点capture，恢复检查 | 状态属于同一合法执行点；不要求每盘计数简单相等，允许捕获应用未完成事务；禁止混合不同capture的盘 | 06/10/13 |
| A10 | RW/RW、RW/RO争用、两个Bundle同卷、路径/inode别名 | mutable generation只有合法单writer且无其他普通挂接；immutable RO可共享；未知writer不接管 | 04/10/16 |
| A11 | 旧RAM恢复到已独立推进卷；exact与clone_conflicts | exact明确冲突；clone返回新卷映射且完整数据；从不默用latest | 10/13 |
| A12 | 全部盘角色、attach/detach/grow、部分扩大失败、ENOSPC/fsync失败 | 已附着盘无遗漏；扩容如实反映阶段；不自动缩回/卸载；不发布坏checkpoint | 10/16 |
| A13 | B与A，双向互联、相同A内部IP、跨私网ACL、重叠CIDR、hairpin、MTU | 源地址和VIP正确；Guest root不能冒用别的attachment；未受管IPv6无旁路 | 08/09/15 |
| A14 | child gate每阶段失败/旧ACK重放；普通进程、容器、loopback/网络并发 | 普通用户态在身份门前不执行；外部授权/设备出口closed；失败child不公开、父不倒退 | 03/09/12 |
| A15 | freezer D态/FUSE/helper kill/cancel；停止普通Agent | 有界失败并确认源thaw或held诊断；无依赖Agent的VMM原语仍可用 | 03/07/09 |
| A16 | 复制parent平台token到child，publicAPI与relay都尝试；rollback被撤权旧token | child不得获得parent身份；public入口不把guest-bound当普通bearer；当前授权不回滚 | 09/17 |
| A17 | System真实PID1、Docker/Compose/k3s受测配置、OCI Agent重启 | systemd是Guest PID1；管理服务关闭不杀VM；重接原workload不执行第二份 | 11/18 |
| A18 | batch部分失败、API回复丢失、同key重试、父删除后Taskadoption | 同child IDs/同capture；成功子不被取消删除；任务不重跑且不再读活父 | 12/14 |
| A19 | 两Bundle并存、不同CPU/profile/缺Bundle/坏chunk、历史corpus | 只用匹配reader；不兼容明确拒绝；无coldboot fallback；旧引用保留 | 01/13/23 |
| A20 | platform/Serve/broker/Worker各自重启；scope迁移中断 | 分别核对实际存活、FD、DB、writer、网络；一种重启成功不能替代另一种 | 04/23 |
| A21 | HTTP/TCP-mTLS/SSH、普通TCP/UDP、private直接流量wake；并发请求 | 先授权/ACL后唤醒；同实例一次恢复；有界队列；不重放可能已送达业务数据 | 15/18 |
| A22 | service revision更新、grant/证书撤销、Desktop/SSH旧epoch | 新连接使用新后端；承诺立即撤销的stream实际关闭；旧execution不可被命中 | 15/17/18 |
| A23 | backup复制时并发GC，恢复到隔离目录，缺key/Bundle/盘 | 完整closure和key恢复材料可验证；坏备份不覆盖last-good；无多VM原子快照假象 | 19/23 |
| A24 | schema2实际fixture迁移、事务中断、旧platform打开新schema | 原子成功或回滚；owner明确；旧binary拒不支持schema；无删库重建 | 02/17/23 |
| A25 | 同Worker两个Serve启动顺序、内存增长、fork/restore风暴 | 不互改全局slice；统一预算、限流、准确共享统计；不靠旧压力guard偷偷kill | 21 |
| A26 | 新UI/TS/Python/CLI与official profile窄兼容 | B默认、四态/健康清晰、unknown能力禁用；旧pause/clone语义仍正确 | 20/24 |

性能优化另有A27：dirty追踪、rebase/read lanes/cache每项比较正确性、首任务和吞吐；未获收益不进入默认。A01–A26不是每次提交全部重跑，按触发列选择；首个稳定完整发行执行整组代表性组合。

## 3. 关键实验如何做得有效

### 3.1 内存与祖先生命周期

测试Guest进程维持已分配页面中的随机标记、打开文件的offset、计数器与可恢复计算状态；预先建立多代，刻意保留尚未访问的冷页和来自祖先的磁盘内容。父删除后再读取所有标记并继续写入，验证孙分支与子独立休眠恢复。只证明父exit后子HTTP能响应几秒不够。

记录原生generation FD/extent及实际独占增长，不只看RSS。支持的宿主上加入swap/private页路径，避免只测全零、已prefault热页的最佳情况。少量稀疏/深层/dirty代表样本即可，不做没有目的的几千场景组合。

### 3.2 故障注入

在intent写入、FD交接中、capture committed后回复前、父Continue确认前后、chunk落盘中、manifest rename前后、STOP_COMMIT后、target writer授予前后、identity ACK/activation中、GC tombstone后设置可控failpoint。每个case核对用户数据/源目标真实状态/引用/writer，而非只检查日志打印。

kill测试只针对新建隔离测试实例/服务。不得为测试deep路线杀现有生产Serve、PVE节点或其他VM。Worker正常重启与断电耐久性是两种验证；未做真实断电测试就不得写“断电安全已证明”。

### 3.3 Guest gate最小实验

先用准确Guest Kernel版本验证coordinator与helper例外、UMH_WAIT_EXEC、同caller thaw、helper被杀、异常D态/FUSE。再加入多vCPU、容器、网络队列、同机socket和virtio设备。记录：请求→GateArmed、最后普通用户执行→重新执行、vCPU实际pause、child identity→用户release。全部普通task被冻结并不意味着kernel worker停了；网络/块/vsock各自需证据。

不能用NMI通知一次成功推导任意捕获点都可安全改IP；不能把普通Agent exec成功当成identitygate证明。managed Standby到capture需确认用户计数器完全没推进。

### 3.4 历史状态corpus

长期保留最小代表集：OCI/native、System/native、fixed_internal、附加盘、CPUoffline slots/RAMgrowth、长Standby、incrementalcheckpoint、含活任务的VMcapture。每个fixture存Bundle/CPU/profile及正确结果。只保存足够小且不含用户秘密的数据，引用闭包真实完整；不能留下无reader的孤立内存文件。

## 4. 性能指标与比较规则

| 指标 | 开始 / 结束 | 能回答什么 |
| --- | --- | --- |
| API接受延迟 | request到202 | 控制面响应；不能叫Fork完成 |
| t_user_quiesce | PrepareGate到GateArmed | freezer准备成本 |
| 源用户停顿 | 最后普通用户执行到thaw后恢复执行 | 用户真正受影响的暂停，包括内核gate阶段 |
| t_vcpu_paused | 全vCPU确认pause到Resume/退出 | VMM冻结窗口；Hibernate可能含整段导出 |
| RAM capture / delta bytes | seal/扫描/copy/remap各阶段 | 首次/后续/dirty成本来源 |
| source closure / release | capture开始到源Continue或RETURN_STANDBY确认 | 源锁持有与后续并发能力 |
| 子VMM ready | KVM/设备恢复完成 | 不等于网络/应用ready |
| identity ready / activated | gate开始到ACK/用户release | B/A身份成本与发布安全 |
| guest management ready | Agent/taskd接管完成 | 可选管理功能，独立于原生状态恢复 |
| first real task/service | 用户请求到真实结果 | 最有意义的可用性指标 |
| durable / hibernated | capture开始到manifest durable / 源退出确认 | 保存与真正休眠成本 |
| 内存/磁盘 | PSS/private、uniquebacking、memcg/current、pagecache、allocatedbytes | 实际密度，不等同Guest配置总量 |
| 并发吞吐 | 固定资源预算下完成任务数/时间与tail latency | Agent整体效率，不能只测单VM最快 |

对照固定：同物理CPU、同PVE嵌套层级、同Worker内存下限、同存储栈、同Guest/kernel/设备布局和相同cache条件；注明CPU频率/超分配、宿主邻居负载与dirty比例。首次cold、warm、首次fork、重复fork、逻辑深层与并发fanout分开报告。至少保留原始分布；少量样本不报有统计意义的p99。

建议最小矩阵：clean与有界dirty两种Guest，首次和后续capture，fanout=1及一个Worker承受的中等批量，3层递归，两个网络模式；优化触及某机制再增加对应样本。dirty百分比、并发数、样本数是实验参数，不是性能承诺。基线建立后用可解释的回归阈值；没有本机数据前不预设“必须5ms”或“提升50%”。

block read lanes默认策略的选择看每VM线程/PSS、随机读tail和并发编译吞吐；不能只看单盘fio。网络对照同时记录throughput、p50/p95、CPU/packet、带小包的实例互访与大流；Nested KVM与裸机不要放同一排名表。

## 5. 构建、工件与发布

### 5.1 构建顺序与原有工具

继续使用上游 `scripts/build-libkrun-linux.sh`、`build-agent-rootfs.sh`、`build-dist.sh` 和provenance/export/init/glibc检查，但由新增deep wrapper固定输入、隔离输出和验证产物。脚本顶部旧kernel注释不是权威：实际libkrunfw Makefile指定Linux6.18.60。上游脚本存在容忍submodule失败、复用FW/预编译库的路径，wrapper必须先严格检查再调用。[构建脚本](https://github.com/smol-machines/smolvm/blob/5bd7c2f8e8e2d29869226a91f766bf4147b5e693/scripts/build-libkrun-linux.sh)

顺序：

1. 固定三仓库和patch registry，严格submodule SHA；检查LFS实际文件，拒pointer占位。
2. 固定builder镜像digest、Rust/Go/Node/Python/编译器版本和lockfiles。首次把成功构建解析出的具体版本写toolchain lock；之后不得漂浮stable/latest。
3. libkrunfw：kernel tarball摘要、完整patch series、config片段merge、olddefconfig、required config校验、内核构建、FW库。Makefile原先直接复制config，新增fragment合并hook要在olddefconfig前执行，不只是把文件放目录中。
4. libkrun/init：BLK/NET、Linux x86_64、GPU=0初始profile，静态init；确保加载的是本次FW。新Guest/kernel变更禁止SKIP_LIBKRUNFW重用旧产物。
5. identity helper、普通Agent、OCI rootfs/System模板、smolvm、resourced；组件摘要全部写RuntimeBundle。
6. 平台/SDK/Web按现有build路径增量编译，生成下游release manifest，引用具体bundle digest；不覆盖现有official runtime-manifest.json的语义。
7. 签名/摘要、完整性检查、隔离Worker窄验收后发布候选包；canary通过再设为新建默认。

示意命令（只在WP-01提供wrapper并核对help后使用）：

```bash
./scripts/deep/build-bundle.sh --source-lock packaging/deep/source-lock.json --output dist/deep-candidate
./scripts/deep/verify-bundle.sh dist/deep-candidate
```

这些是待实现命令，不是假称当前仓库可直接运行。现有脚本内部安装系统依赖只允许在专用builder/容器，不在运行生产VM的Worker随意apt升级。CPU/内存最大构建并发独立限制，避免内核编译挤压PVE中的运行VM。

### 5.2 CI分层

普通CI做schema/编译/SQL/静态；KVM功能测试用专用self-hosted Worker，明确nested层级。远端不可信PR不能获得生产Worker密钥或高权限；产物进入候选registry前有来源/摘要验证。Linux deep功能可cfg-gate，保留其他平台原有编译路径；不要求自研特性全移植macOS/Windows/ARM。

每次发布包括：Bundle manifest、二进制与rootfs摘要、源码/patch摘要、LICENSE/NOTICE、capability验证报告、兼容reader列表、部署模板、变更说明。当前工程包baseline-lock只锁源，binary_artifacts标not_built；不能拿它充当运行时发行manifest。

## 6. 部署隔离与PVE边界

### 6.1 路径和服务约定

| 对象 | deep路线 | 官方路线处理 |
| --- | --- | --- |
| Runtime工件 | `/opt/smolvm-custom/bundles/<digest>/` | 原路径保持 |
| platform工件 | `/opt/sandbox-platform-deep/releases/<version>/` | 原current symlink不改 |
| 资源catalog/对象/卷/快照 | `/var/lib/smolvm-custom/worker/` | 不共享可写对象池 |
| Bundle Serve状态 | `/var/lib/smolvm-custom/engines/<digest>/` | 不打开原Serve DB |
| 平台DB/config | `/var/lib/sandbox-platform-deep/`、`/etc/sandbox-platform-deep/` | 不打开原platform DB |
| UDS | `/run/smolvm-custom/resourced.sock`、`engines/<digest>/serve.sock` | 独立路径/权限 |
| systemd | `smolvm-deep-resourced.service`、`smolvm-deep-serve@.service`、`platformd-deep.service` | 不stop/restart原unit |
| VMM cgroup | 新deep专用slice和全局唯一scope ID | 不改现有smolvm.slice预算 |
| Gateway/DNS/hostports | deep显式配置、启动前端口冲突检查 | 不抢已有监听；示例端口不强占 |
| 私网/IP池 | route_id=deep专用reservation，独立router/port netns | 不使用另一条路线分配池 |
| build/test | 单独target/dist/cache和带test前缀对象 | 不清理共享缓存/用户数据 |

只有custom的多个Bundle共用一个resourced/catalog；官方/Cocoon没有接入协议时采用独立资源池和可证明的配额预留。不能靠两个独立platformDB的唯一索引实现跨运行时排他。

### 6.2 实际cgroup与服务重启

平台现有unit使用KillMode=control-group，但当前smolvm已通过`StartTransientUnit/adopt_into_scope`把VMM移入独立scope；不能从unit一行就断言Serve重启必杀VMM。真正要检验正常scope迁移、迁移中断、fallback、guardian/holder线程的实际`/proc/<pid>/cgroup`。[scope源码](https://github.com/smol-machines/smolvm/blob/5bd7c2f8e8e2d29869226a91f766bf4147b5e693/src/systemd_scope.rs)

该文件`VM_SLICE="smolvm.slice"`是硬编码，多Serve可能互改同slice限制。deep必须参数化为自己的slice，预算只由一个Worker owner配置，scope名包含route+全局instance ID。VMM不放进Serve/broker service cgroup，不对其设会级联杀VMM的PartOf；broker重启不等于重启所有VM。只改KillMode=process会留下残留进程问题，不能作为完成标准。

`read-runtime-status`、`grant-runtime-socket`、`smolvm-status@.service`也有固定路径/版本，必须改为可信runtime-instance配置生成，不能接收用户任意binary/socket路径。非systemd fallback若不能提供同样承诺，profile明确不支持Serve无损升级。

### 6.3 现场只读预检

在实际Worker先获取以下信息，并与管理员已有配置核对。下面均为只读示例：

```bash
uname -a
lscpu
lsblk -J
findmnt -J
ip -j address show
ip -j route show table all
ss -lntup
systemctl list-units --type=service --state=running
systemd-cgls
```

另检查`/dev/kvm`可用、KVM capability/嵌套支持、cgroup v2/memory.current/PSI、目标磁盘free bytes/inodes、kernel是否满足所选可选UFFD后端。默认shared Fork不依赖开启kernel-fault userfaultfd；不能为可选后端任意放开宿主安全设置。

历史环境记录是VM107/Ubuntu24.04、8vCPU、12GiB且Balloon下限8GiB、ext4 `/mnt/smolvm-data`；这是历史而非当前探测。物理PVE管理网`10.10.0.0/24`，PVE`10.10.0.254`、Mihomo`10.10.0.1`、Tailscale管理保持。扫描`100.64.0.0/10`、Mihomo fake-IP`198.18.0.0/15`、Docker/k3s等现有route；不得默认分配这些冲突池。

不格式化已有测试盘，不改PVE vmbr、物理宿主kernel、默认网关或现有Tailscale子网路由。只在Worker自己的namespace及带deep ownership标签的nft表/链中配置。需要L1转发时明确限定项目接口/规则，不全局关闭rp_filter或清空防火墙。数据盘原backup=0和共享thin pool不能当独立灾备。

### 6.4 初始资源预算

effective Worker预算以真实cgroup/PVE Balloon下限和保留量计算，不只用名义12GiB。deep最大可用预算需扣OS/控制服务保留、其他路线可证明的上限/预留和RAM导出/恢复峰值；现场未确认其他路线约束时不做高密度压力测试。`MemAvailable`/PSI用于持续校正，不能假称能约束未注册任意进程。

硬限制加保守准入防止失控增长；共享memfd/pagecache的memcg计费通常留在首次charge的cgroup，FD转移不迁账。查看共同ancestor和已移出的charge，父退出不代表共享页释放。Standby主要停CPU/设备，不释放大部分RAM；真正内存压力缓解需要回收缓存、balloon或durableHibernate。不得自动以SIGKILL兑现“回收成功”。

## 7. 安装、升级、回退

### 7.1 初次安装

1. 创建deep独立账户、目录和服务，检查属主/权限。resourced受限root只处理白名单协议；platformd普通用户。保留VMM原有per-VM uid/seccomp/Landlock。
2. 安装并验证不可变Bundle；配置Serve的`--state-dir`/SMOLVM_DATA_DIR、SMOLVM_LIB_DIR、SMOLVM_AGENT_ROOTFS及独立socket；不要改HOME伪装隔离。
3. 启动resourced，catalog无未知writer后启动候选Serve与平台；注册profile但初始未验证能力unknown。
4. 在单独测试网络/卷创建OCI和System小实例，跑该候选必需门。检查实际加载.so路径、GuestKernel和manifest一致。
5. 启用新建默认profile，保留旧Bundle/官方路线；新网络默认B，A显式可选。不会自动迁移旧VM。

### 7.2 Runtime升级

追加新Bundle→静态校验→注册reader/capabilities→单独Serve DB/socket→指定测试实例canary→更新“新建默认”指针。运行中实例继续旧Bundle。Hibernated/Checkpoint按存档Bundle或明确兼容reader恢复；不可因新版提供相同API就换二进制读旧blob。

未知CPU/KVM/serializer不兼容时拒绝目标启动，保留对象；不能让用户以为快照丢失后只能冷启。保留旧reader有安全成本，见§9。设备布局/DAX默认值变化为新profile，旧profile不重写。

### 7.3 platform和catalog迁移

升级前取SQLite在线一致备份、配置/secret key恢复材料、当前control epoch及Bundle/状态引用清单；备份不是直接copy一个正在写WAL的DB主文件。migration只修改schema和投影，不顺手resume VM、换IP或改卷。

SQLite CHECK改变使用新表复制/核验/索引外键重建/原子换表。当前migrate在事务中且foreign_keys开启，事务内PRAGMA foreign_keys=OFF不生效；不能照搬失效迁移脚本。验证foreign_key_check、rowcount/引用和schema2真实fixture。多principal后bootstrap与自动调度主体都查询安装级owner/内部system principal，不再LIMIT1随机挑一个。

旧platform二进制启动检查schema范围；回退使用匹配DB备份和新的control epoch fencing，避免旧客户端重放修改。若新版本已产生新用户数据，不能直接把旧DB覆盖回去丢数据；选择forward fix或显式迁移/导出，破坏性方案须给用户审查。

catalog支持旧/新Bundle协议兼容窗口；只有停止所有相关写者并有备份时才做破坏性schema收缩。普通更新不把新Serve指向旧Serve私有DB。

## 8. 故障恢复手册

| 观察 | 首先做 | 可以做 | 禁止做 |
| --- | --- | --- | --- |
| platformd不通，VM服务仍活 | 读日志、DB锁/control epoch，检查Serve/broker健康 | 重启deep平台，reconcile持久op | 以API不通认定VM已停、重跑用户任务 |
| Serve退出 | 检查VMM pid/starttime/boot_id/scope、broker inventory | 重启相同Bundle Serve，接管原执行 | 起第二VMM占相同网络/卷 |
| broker退出 | 保留VMM、禁止GC/新writer；检查catalog/WAL | 重启、按FDprovider重新登记，标lost闭包 | 清空leases或按TTL转移writer |
| capture/source held | Query op与VMM状态，确认decision/映射/盘是否完整 | STOP前安全abort或继续；STOP后只完成exit | 断连就Resume、SIGCONT绕过状态机 |
| child identity失败 | 保持quarantine，保存ACK/nonce/profile诊断 | 同capture合法重试或终止候选释放refs | 开旧IP、换A、coldboot伪成功 |
| writer不明 | 查pidfd、后端句柄/I/O/fence与catalog | 获得可靠停止证据后接管 | 只改epoch/断网/删锁文件 |
| 内存压力 | 查实际current/PSI、活动/lease、预算、自动决策记录 | 停止新扩张、受控reclaim/balloon、合格Hibernate | 绕过keep_running/活动保护杀用户VM |
| checkpoint缺对象/损坏 | 校验closure/reader/key，保留日志 | 选上一份有效状态或备份，精确报告不可恢复部分 | 缺块当零、用当前盘替代历史盘 |
| Worker重启 | 新boot_id、旧volatile全部失效 | 验证durable并按恢复队列唤醒 | 用旧PID“接管”不存在执行 |
| 旧Bundle缺失/被撤销 | 查引用、安全级别和已验证reader | 恢复工件、隔离读取/导出或修reader | 删除唯一历史状态或自动公网运行漏洞包 |

日常诊断报告包含profile、operation阶段、source/target disposition、resource closure、writer proof、gate状态、网络attachmentrevision、磁盘/内存generation、clock合同和bundle摘要。Secret/完整环境变量/Token/SSH私钥不进报告。用户可在UI看明白“实例在运行，管理Agent不可用”与“恢复失败”不同。

## 9. 长期上游同步与历史状态保留

### 9.1 同步工作流

1. 获取smolvm及其指定gitlink的上游提交，另观察smol-machines依赖HEAD；不能`git submodule update --remote`自动换依赖。记录commit和tree差异。本次FW两commit同tree就是需要处理的正常情形。
2. 对变化按memory/capture/device/network/init/security/API/build分类，识别新增ABI/格式/默认布局、删除接口和依赖绑定。
3. 与下游patch registry比对：已由上游实现的功能应在candidate branch删除对应patch，重新跑该能力门；保留为什么等价和何时能移除旧兼容reader。
4. 在隔离candidate worktree解决冲突，保持已发布deep分支/标签不被强推重写。更新依赖先出固定commit，再更新父gitlink和Bundle。
5. 构建新Bundle；按修改面执行L0/L1/L2，保存旧corpusreader结果；性能相关做小对照。生成具体变更摘要和可审查差异。
6. canary用于新建/可丢弃实例；合格后可在既有授权范围内推进新建默认。运行/休眠/历史状态的Bundle引用不自动改变。

可由Codex周期性执行检查/候选构建/同步PR和摘要；repo CI定时或workflow_dispatch按部署者安排，本次不创建额外聊天自动化。普通无语义变化的冲突修复和可逆candidate验证不逐项求批准；核心语义/历史承诺/破坏性迁移才进入人工边界。

### 9.2 补丁记录与维护成本

每个patch：ID、purpose、upstreambase、依赖、触及hook、受影响profile、必要验证、upstream替代条件、撤除步骤。kernel补丁独立series/config，不和格式化/搬目录混合。记录每次冲突热点、需重复验证的范围及实际耗时；某优化长期改大量核心路径而收益不明显，应主动移出默认。

不预造永久通用VM格式。短期用精确bundle+reader envelope，长期按需要增加显式转换器。旧reader仍需受约束运行；用旧runtime恢复再checkpoint并不会自动升级格式。历史对象GC看refs，不能简单“保留最近两个版本”。

### 9.3 安全与旧版本

Bundle状态active/legacy-allowed/quarantined/revoked，记录原因和受影响能力。严重漏洞旧包不自动公开入口；有价值历史状态可在隔离网络/只读数据导出环境恢复，或开发受验证reader转换。不能为了“能恢复一切”永久自动运行未隔离漏洞栈，也不能未经用户决定毁掉历史checkpoint。

## 10. 发行完成清单

- NativeB默认与FixedA可选、四态、原生独立Fork、batch/递归、多盘完整状态、System/OCI均有匹配profile证据。
- Guest管理健康、workload_error、network identity和service readiness分别展示；后台Taskadoption不冒充重跑工作负载。
- 活动/授权/压力控制、Secrets/凭据代理、SSH/Desktop、MCP/事件/自动备份均经统一API与现有客户端可用。
- 当前没有supported能力缺少对应验证；所有真实限制可查询，未完成项不伪装降级成功。
- 新旧Bundle、schema迁移、服务/Worker故障恢复、备份闭包与回退流程有实证。
- 统一自动生命周期的全局/实例策略、活动/LLM等待保护、运行租约、压力回收与wake竞态已经通过A28–A36；auto_hibernate=false等策略不限制用户主动LiveFork。
- 只修改deep自己的目录、服务、网络对象和配额；其余路线和PVE管理网络保持可用。
- Release说明明确已实现/未实现/未实测；性能有实验条件和原始数据，不用竞品宣传值充SLA。

## 11. 初始工程计划的验证边界

2026-11-25 的初始工程计划核对了 Git 源码、依赖 tree、本地开发分支和文档协议，归档中有五份主文档、源码基线及摘要。次日的实现验收记录了具体安装和测试环境。整机断电用例的边界为独立虚拟 Worker 的电源。[初始基线](README.md) · [虚拟 Worker 换 boot 实测](managed-recovery.md#正式安装的整机复测)


## 12. 自动生命周期专项验收与运维

这是用户最新明确要求的正式发布门，规格以TECH §15为准。custom路线在resourced内只有一个WorkerLifecycleController；platform监控与各Bundle Serve不再独立挑选sleep/kill/resume。旧上游压力guard必须拆出telemetry/reclaim并替换自主动作，不能只设置关闭环境变量就算完成。策略下发、运行租约和Access活动保护都要求Worker确认，不能平台返回成功而Worker尚未生效。

### 12.1 全局默认、实例覆盖和客户端

初始policy示例：自动Standby 300秒、自动Hibernate 1800秒、keep_running=false、priority50、pressure_reclaim=idle_only、wake后最小运行60秒。这些是可配置产品策略，不是性能实测或强制SLA；两timeout从同一连续idle起点算。global默认在新建时快照化，既有实例通过显式批量应用更新，避免设置修改立刻使正在运行机器睡眠。UI同时展示desired/appliedrevision、默认来源与override。

auto_hibernate=false禁止包括pressure在内的所有自动Hibernate；keep_running阻止自动待机/休眠。它们不禁止手动Hibernate/Checkpoint/Fork。priority越小越先被回收，但活动/lease/unknown保护优先；它不是CPU权重。Fork继承源有效policy，不复制其lease、活动时间或自动operation，子激活有新的保护窗口。

沿用现有`settings patch`、`machines patch`以及TypeScript/Python方法，增便捷flags和policy状态读取。WebUI全局设置与实例页必须能配置timeout、常驻、priority、pressure和wake条件，能显示“等待模型响应”“suspect lease”“活动未知”“存储不足”等阻塞原因；只有JSON后台字段不算完成产品。

### 12.2 新增必要验收

| ID | 测试 | 通过标准 |
| --- | --- | --- |
| A28 | Running→Standby→Hibernate并超过Guest collector普通TTL、只开其中一个、全局默认/实例覆盖/批量应用 | 同idle起点计时；受控暂停collector在hold持续可证且host监测有效时不阻断第二级休眠，hold证据丢失则block；不支持的profileblocked；实例下发ACK前pending；旧请求保持原仅Hibernate语义 |
| A29 | CPU近零等待LLM响应/SSE、活跃SSH/PTY/Exec、lease、普通空闲监听daemon | 等待中的Agent不被睡；真正idle可睡；unknown明确block；不只看CPU或无包间隔 |
| A30 | keep_running/续租/新Access与autoaccept、dispatch并发 | 同一Workeractor排序；已成功applied的保护不被旧自动候选覆盖 |
| A31 | 从Running/Standby开始Hibernate，在STOP_COMMIT前后到达多个授权wake/private packet | 前期abort回原态，原Standby随后走正式Wake；后期finish再唯一restore，逻辑WakeDemand重授权后原子绑定新execution hold才放行访问；没有丢请求、重复restore或coldbootfallback |
| A32 | v1 suspect与v2 expired、旧epoch续租、child复制父lease token | v1行为兼容，v2释放不证明idle；旧执行/child不能续父；UI能解释阻塞 |
| A33 | 多Bundle同时压力、新建/restore风暴、共享backing最后引用/仍有活引用、全部候选受保护 | 只有一个controller；先限流/reclaim/balloon，eligibleHibernate；无隐性kill；durable根切换后释放冗余memfd，保留其他活引用；报告实际回收量而非配置RAM/RSS总和 |
| A34 | platform/broker/controller重启，丢ACK与旧activityseq重放 | policy/lease/holds对账；stale丢弃；unknown保守；自动op不重复副作用，wake_pending可恢复 |
| A35 | auto_hibernate=false、keep_running=true、有效运行lease和等待LLM的源主动Fork/Batch | 原生完整Fork可执行，任意活动进程被复制而非重跑；只受真正预算/设备/授权/锁约束 |
| A36 | 禁隐式wake、开启特定触发器、Stopped/Hibernated区别、明确手动Stop | 条件准确；explicit wake仍可授权调用；Hibernated失败不冷启；旧pendingwake不反转手动Stop |

定时器逻辑可用受控clock的单元/集成fixture加少量真实Worker路径验证，不为了5分钟/30分钟默认值在每个case真实等待。native时钟恢复与Guest停顿测试仍按A08做真实验证，不用调度器fakeclock取代。

### 12.3 故障和压力操作

自动睡眠没发生时，先看lifecycle_status.blockers和collector freshness，而不是强制关闭所有保护。有效的低CPU模型请求是预期阻塞原因。保守保护的空闲TLS keepalive可由协议感知proxy区分；任意加密连接不能无证据判定没有in-flight请求。UDP和无法识别的工作负载需要明确lease/受测activitypolicy，不能假定“最近没包”就安全。

Standby内Guest collector正常冻结显示controlled_suspended；只有相同execution的gate/设备hold持续可证且host入口/网络/lease监测实时有效，才沿用静止快照。它不等于unknown，也不允许清掉待机前的busy/unknown。hold证据丢失或未解释的恢复要重新取证并阻止自动动作，不能仅延长所有collector TTL掩盖故障。

压力回收顺序为限制新增需求→有界cache reclaim/balloon→合格实例Hibernate。Standby没有释放大部分RAM，不能计作足够回收。导出本身需要缓冲/空间/I/O和可能的generation峰值，预留不足时不要反复启动必失败Hibernate。每次完成后重测pressure，采用high/low水位与cooldown，避免连续睡/醒抖动。

无合格候选时保持准入关闭、告警和可诊断状态。真实OOM/硬limit导致退出必须标明确故障，不能包装成Hibernate成功；常驻和lease不保证无限内存。platform暂离时Worker仅按最后appliedpolicy和新鲜证据动作；缺activity不自动睡，未经授权的新网络需求不盲目唤醒。

### 12.4 准入与Fork资格必须分开

正常内存/盘预算拒绝可以影响任意创建操作，包括主动Fork；**自动回收优先级、auto_hibernate关闭、keep_running开启、正在运行任务或租约本身不能拒绝主动LiveFork。** queued自动操作让位于手动需求；已经进入不可逆状态按事务正确完成后再处理，不绕过一致性。验收报告必须单独列出A35，不能只证明在空闲源上的Fork成功。
