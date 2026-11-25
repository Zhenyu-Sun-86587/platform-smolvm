# Phase 1 本机验收

2026-11-25 在 pve-bot 上开发并测试。运行时为官方 smolvm 1.37.1，管理面通过私有 Unix socket 调用。基础环境为 Linux amd64、KVM、systemd、cgroup v2。Web、CLI 和两个 SDK 均调用同一服务。发布模板的验证范围按镜像 SHA-256 记录。[模板能力清单](../../packaging/managed-template-gates.json)

## 阶段与证据

| 阶段 | 本机观察 | 记录 |
| --- | --- | --- |
| M1 管理工作区 | SQLite 受理和 generation、登录、创建、恢复及删除。拦截原生成功回包并强制终止 platformd 后，原操作继续，原生只收到一次 Create。 | [生命周期与故障注入](../../tests/evidence/m1.md) |
| M2 任务与恢复 | Guest journal 的游标与退出收据可以重连；Live Fork 接管原 runner；独立恢复能读取 RAM 与文件 canary；回滚在身份不足时保留锁和制品。 | [Guest、快照和回滚](LOCAL_GUEST_EVIDENCE.md) |
| M3 服务与 idle | 20 个请求合并唤醒；SSH 主机密钥验证、SFTP、浏览器预览、mTLS 半关闭、撤销先于 wake；实际 idle capture 期间的首个客户端断连仍能完成唤醒。 | [入口和 idle](../../tests/evidence/m3.md) · [客户端](../../tests/evidence/clients.md) |
| M4 客户端与交付 | Chromium 完成登录、终端、文件、任务与预览；CLI 和 Python/TypeScript SDK 完成创建、日志重连、快照及独立副本。安装、升级、回退、备份、端口权限与冷启动在专用测试安装或本机嵌套 KVM 中演练。 | [客户端](../../tests/evidence/clients.md) · [安装与恢复](../../tests/evidence/w01-w11.md) |

## 产品验收覆盖

| 编号 | 实测范围 |
| --- | --- |
| A01–A02 | 重放原 idempotency key，冲突指纹拒绝，Create 已生效但回包被拦截时重启；修正 status socket 后从原导入候选继续 restore/clone。 |
| A03–A05 | HTTP 并发扇入，SSH 与 mTLS 单独验证唤醒；idle 派发期间断连后保留 wake_pending；后端处理 POST 后断开，业务计数为 1。 |
| A06–A07 | 跨对象令牌与证书、撤销、认证先于唤醒；真实 OpenSSH、SFTP、WebSocket、TLS 半关闭、Guest root 文件和 4 MiB 二进制传输。 |
| A08–A10 | 在 Guest spawn 窗口中断 taskd，启动一次；宿主丢失回包后找回原任务。日志洪水、多 reader、输出缺口和保留期分模板验证；重启 serve 后原生 exec 明确报告 session_lost。 |
| A11–A12 | 低 CPU 任务和过期 suspect lease 均保留 hold，指标读取保持原活动时间。原生 memory-pressure leaf 实际进入 paused；平台仅在足够新鲜的证据下更新任务状态。 |
| A13–A16 | 父子依赖、实时分支接管、两边任务与文件隔离；源数据不可访问时独立恢复；冻结任务默认中断与显式继续。 |
| A17 | 运行、已停止、普通暂停与身份冲突走过产品回滚流程。created 分支有针对性测试；压力暂停 leaf 的导出、Stop 冲突和无级联 Delete 在原生运行时直接验证。 |
| A18–A19 | 归档路径及长度、签名与 SHA 校验、信任隔离、引用 pin 阻止删除、独立运行时恢复。临时调高磁盘阈值后，新建操作以 DISK_LOW 结束，没有派发原生 Create。 |
| A20–A22 | 浏览器刷新与 token 撤销、SDK 日志游标、安装服务实际 restart/SIGKILL 和 stop timeout。嵌套宿主重启后原任务先 unknown，显式启动 Guest 后得到 interrupted 收据。数据库恢复更换 epoch，并演练回退与保留数据的卸载。 |

## 部署边界

管理面默认监听本机回环地址。发布安装与配置说明包括 TLS、应用域名、mTLS、备份与维护流程。模板以 Debian managed r3 为默认版本；终态日志回收的完整验收在可选 r4 上完成。[安装说明](../../packaging/install/README.md) · [Guest 验收](LOCAL_GUEST_EVIDENCE.md)

已将签名备份从 ext4 系统盘复制到独立挂载的 virtiofs，并在隐藏源路径后验证。物理存储池或站点故障域的独立性仍需基础设施侧提供放置记录。压力暂停和 created 回滚的端到端覆盖范围见 A17。[备份实测](../../tests/evidence/w01-w11.md)
