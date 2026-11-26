# sandboxctl 工作流与 boxd 命令对照

本页采用 boxd 0.3.8 的 Linux 实验、CLI Synopsis 和原始帮助作为对照。命令中的 `MACHINE` 是平台机器 ID。支持名称的入口接受 `name:完整显示名称`；名称有空格时加引号。解析必须唯一，脚本应保存创建回执中的 ID。机器以外的资源使用各自 ID。

| 工作流与 boxd 实验证据 | sandboxctl 用法 | 默认行为、保证与边界 |
|---|---|---|
| `machine new/get/list/rename/config/resize`。E14 的一次 resize 更换了 boot ID；E31 的跨资源事务由 SDK 实测 | `machines create/list/show/rename`、`machines config get/set`、`machines resize/resources/metrics` | 创建默认启动。System 使用注册的模板和明确的 `--runtime-instance`；资源调整受当前 capability 限制。分别读取请求值、已应用值及带时间和身份的观察。配置入口只接受平台支持的字段。 |
| `machine exec --tty`、`connect`、`machine cp`。E05 核验 TTY、SSH、字节往返 | `exec --follow --cwd / MACHINE -- argv…`、`exec --tty`、`ssh MACHINE`、`files list/put/get` | 原 exec 默认只等初始输出，长任务用 follow。System 默认目录可显式设为 `/`。文件用绝对 Guest 路径，下载到新文件或 `-`。公开上传 64 MiB，按执行实际能力判断；历史私有上传上限 512 KiB。 |
| `machine fork`、`machine new --fork`。E01/03/04/08 观察到 Guest 内存、进程和既有 loopback 连接继承；E19 验证删除祖先后的后代 | `machines live-fork MACHINE CHILD`；兼容入口 `machines fork MACHINE CHILD --require-independent` | 强语义入口要求原生完整运行态和独立资源。原 `fork` 的独立性开关默认关；`clone` 有独立的复制合同。应用绑定旧 IP 的 socket 可能无法在子机新 IP 上接收连接。 |
| E30 对多条单机 fork 命令做 1/2/4/8 并发 | `machines fork-batch MACHINE CHILD… --admission all_or_reject`；`operations retry OPERATION` | 平台 batch 在同一捕获点建立独立子项，支持 `best_effort`，最多 32 项。检查 `children`、`effect` 和 attempt。retry 使用原捕获中合资格的失败子项，保留成功子项。 |
| `machine checkpoint save/restore`、`snapshots save`、`machine new --from-snapshot`。E09–E11、E26 分别验证同机回滚、新机恢复、外接盘与故障样本 | `snapshots capture-vm MACHINE`、`rollback-vm MACHINE SNAPSHOT`、`restore-vm SNAPSHOT NAME` | 三个入口限定 VM 模式。回滚要求同源机器，切换 execution 并返回 rescue。新机恢复有独立机器 ID。`--volume-restore-policy exact` 遇代际冲突终止，默认 `clone_conflicts` 保留冲突写头的独立性。读捕获时的 `capture_summary`、恢复的 `volume_map` 和目标 `machines resources`。历史缺摘要为 unknown。 |
| 磁盘附加、网络选择、发布入口。E06/07 对照网络隔离和 Guest 管理；E26 验证盘内容；E32 记录 TLS 延迟、授权拦截和无监听的 502 | `volumes`、`volumes attachments`、`volumes snapshots`、`networks`、`machines network`、`services create/status/endpoint`、`port-forward` | 卷记录实际 attachment 和代际；离线 attach/detach 遵循资源状态。网络为已注册的 Worker 私网。服务配置完成后可先为 unknown；最近 upstream 观察只覆盖该 revision、execution 和阶段，实际公网可达性需用相应的凭据从目标入口请求。 |
| `machine pause/resume`、`hibernate/wake`、`stop/start`。E24 慢出站请求在待机后超时；E29 冷启保盘并丢失易失状态 | `machines standby` + `machines wake-full MACHINE --from standby`；`hibernate` + `wake-full --from hibernated`；`stop` + `cold-start` | Standby 恢复同一 execution。Hibernate 使用保存的 RAM 恢复到下一 epoch。冷启从磁盘启动。旧 `pause/resume` 保留既有分派；旧原生 `wake` 接 Standby，`start` 在 Hibernated 时恢复保存态，在 Stopped 时冷启。 |
| 自动态、入站与出站策略。E23/24 区分被动观测、实际访问和低 CPU 等待，E29 测量超时和状态变化 | `lifecycle defaults get/set`、`lifecycle status/set`、`leases acquire/list/renew/release` | 结合 desired/applied revision、blockers 和 lease 阅读。v1 默认 300 秒，到期 suspect 仍保护；显式 `--semantics-version 2` 到期释放。续租/释放需要原 token 与当前 revision。模型等待可持有运行租约。自动睡眠资格独立于用户主动 Live Fork。被动查询保持 idle；显式文件、终端、服务访问沿用各自 access 策略。 |
| boxd 的命令回显、`--json`、原始 exec 输出及 E30/E32 的分阶段结果 | `--json`、`--no-wait`、`--wait-timeout`、`operations get/wait/retry` | 受理保留原 operation ID。等待操作完成后，另用 exec 证明 Guest 就绪，用真实协议证明服务可达。未知副作用保持 unknown。轮询超时退出 124，只查询原 operation。 |

旧 `snapshots capture` 默认 `--purpose clone`、managed_tasks。显式 `--capture-mode vm` 仍有效。旧 `snapshots restore/rollback` 按保存点记录的模式分派。归档的 verify/export 和安装级离线 `admin backup restore` 各有自己的范围，不能用作 VM 状态点或按机冷恢复的替代入口。

### 最短可运行示例

在具备 Native System 能力的隔离平台上，使用有权创建机器的 token。把候选二进制放入 PATH，设置 `SANDBOX_URL`、`SANDBOX_TOKEN`、`TEMPLATE`（System 模板 ID）和 `RUNTIME_INSTANCE`（已注册的 Serve 实例 ID）。使用自签 CA 时另设 `SANDBOX_CA_FILE`。下面命令需要 Bash 和 jq。

```bash
set -eu
: "${SANDBOX_URL:?}" "${SANDBOX_TOKEN:?}" "${TEMPLATE:?}" "${RUNTIME_INSTANCE:?}"
accepted=$(sandboxctl --json --no-wait machines create "cli-demo-$$" \
  --boot-mode system --template "$TEMPLATE" --runtime-instance "$RUNTIME_INSTANCE" \
  --cpu 1 --memory-mib 512)
op=$(jq -er .operation_id <<<"$accepted")
mid=$(sandboxctl --json operations wait "$op" | jq -er .machine_id)
sandboxctl exec --cwd / --follow "$mid" -- /usr/bin/id
dir=$(mktemp -d)
printf '%s\n' 'from sandboxctl' >"$dir/input"
sandboxctl files put "$mid" "$dir/input" /root/cli-demo.txt
sandboxctl files get "$mid" /root/cli-demo.txt "$dir/output"
cmp "$dir/input" "$dir/output"
sandboxctl machines delete "$mid" --yes
rm -f "$dir/input" "$dir/output"
rmdir "$dir"
```

如等待超时，保留 `op`，用 `sandboxctl operations wait "$op"` 恢复观察。若连接在写入受理前后断开，保留原 key、epoch、revision、generation、目标与参数，先查询。文件提交返回 `OUTCOME_UNKNOWN` 时，核对同一执行的字节或哈希，禁止创建新的写入来推断前次结果。对 Exec 区分 API 失败与 Guest 自身的非零退出。PTY 与 SSH 为终端原始流，二进制下载到 stdout 时将日志留在 stderr。

### 脚本如何判断下一步

| 场景 | 输出与退出码 | 脚本动作 |
|---|---|---|
| 已受理，带 `--no-wait` | stdout 的 JSON 中有 `operation_id`，退出 0 | 持久保存 ID，以 `operations wait` 查询。 |
| 已到终态 | stdout 保留 `state/stage/effect/children`。旧脚本遇到 `succeeded` 且 `partial` 仍退出 0，`--strict-result` 对 partial/unknown 退出 1 | 自动化用 `sandboxctl --json --strict-result operations wait "$op"`。读取各 child 后，按原 operation 决定是否重试。 |
| 管理错误、能力不支持、revision 冲突 | 退出 1；`--json` 写一个 stderr 错误对象，按响应保留 `request_id/code/effect/retry`，能力错误另有 `capability/reason_code` | 检查当前版本和权限。重新读取资源、revision 和 execution，再决定是否提交不同的用户意图。 |
| 观察原 operation 超时 | 退出 124；`error.operation_id` 和 `retry=poll_operation` | 只继续查询该 ID，不再创建。 |
| 写入回执丢失 | 退出 1；不确定时 `effect=unknown`，有原请求信息时在 `error.intent` 内返回 | 保留原始意图和实际目标证据；HTTP 写入和 Guest 文件写入都不自动重放。 |
| 非交互 Exec 或交互终端 | `exec --follow` 分别写 stdout/stderr，返回已确认的 Guest 退出码；`--json --follow` 是逐页 JSON。TTY 合并终端字节并发送 stdin/resize | 对脚本透传程序退出码。恢复输出读取用原 `exec_id` 和 cursor。SSH 的远端命令使用 OpenSSH shell 语义，保留 argv 用原生 exec。 |

在可丢弃的现有 System Guest 上，以下命令演示显式状态保存。运行前设置 `MID`，回滚会替换该机器的当前运行态。

```bash
set -eu
: "${MID:?}"
sid=$(sandboxctl --json snapshots capture-vm "$MID" --label cli-test | jq -er .result.snapshot_id)
sandboxctl snapshots rollback-vm "$MID" "$sid" --volume-restore-policy clone_conflicts
restored=$(sandboxctl --json snapshots restore-vm "$sid" "cli-restored-$$" | jq -er .machine_id)
sandboxctl exec --follow --cwd / "$restored" -- /usr/bin/id
sandboxctl machines delete "$restored" --yes
sandboxctl snapshots delete "$sid"
```

跨资源 Machine Spec 事务、完整 AgentSession、商业功能及内核 syscall 后置。多节点有独立设计、分支与验收。boxd E25 的 Agent 回合认证失败，E27 的域名继承和 E28 的黑屏截图也各有证据边界。
