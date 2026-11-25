# 冻结候选 91e2c72 的安装与 KVM 验收

2026-11-26 在隔离的 Linux x86_64 Worker 上验证了候选包 `sha256:512366a367c3f424d20c787b2144d81b1719128468212b69ef4db078e1821ffc`。本次验收覆盖 OCI、Debian System 和下表中的四项 managed 合同。后续私网唤醒、备份导入和资源压力改动需要用各自的新候选包复验。

| 构建来源 | 固定值 |
| --- | --- |
| smolvm | `91e2c72ae094918802aaba96351bff55f5ffc735` |
| libkrun | `7d01c1cd36a653591d761437f3332595bfd3c6a6` |
| libkrunfw | `0cbb229e7b55698028e78fef9afe9c32797d0b3f` |
| 构建环境 | Debian 11，构建镜像摘要 `sha256:9a40b3c2b16cc8a1fcb22d64482b627cd53b3b371ae8c789ff0f4943fcaed149`，glibc 符号上限 2.31，`GPU=0` |
| Debian System 盘 | 805306368 字节，`sha256:ad9afc654e73f8640ba5859f2079dc18c66ab4fa73d64c4217338babb1860d7b` |
| 验收工具 | 平台 `b2b387a9f67211f8055f565402e688f2d040eaca`，smolvm 验收夹具 `9eba0dbe9a5d62c578d0fd9f68282903205f9efe` |
| managed 验收记录 | `metadata/managed-acceptance.json`，`sha256:89df82087f92fdc8577239fbb29ff4d8073465ba66aeaddc9aa2cbfbf73b467c` |

| 验收项 | 实际观测 |
| --- | --- |
| OCI 启动与安装 | 构建目录及独立安装目录各启动两台 OCI Guest。安装、再次验证、安装后的 Serve 握手均得到本页的包摘要。 |
| `managed_block_launch` | Debian 根盘经真实块设备启动，受管 Agent 通过 Worker 核验的 vsock 提供访问。宿主普通连接无法取得 Guest 身份。 |
| `managed_parked_fork` | Debian System 分叉产生独立的 Agent socket 和 CID。OCI 分支在同一捕获点恢复用户进程与两块可写盘。 |
| `managed_statepoint` | OCI 恢复延续原进程计数，回滚保留救援点，另一逻辑实例读到同一 RAM 和双盘边界。Debian 连续保存后恢复到新的 CID。 |
| `lifecycle_policy` | 活动中的出站连接保护低 CPU 的 Guest。活动归零后进入 Standby，再进入 Hibernate。两个获准的唤醒需求共用一次恢复，Worker 在交接执行权前重验授权。 |

验收记录逐项保存五个夹具和日志的 SHA-256，并绑定实测的 `platformd`、Guest 计数器、vsock 探针及构建记录。安装结果、两份 OCI 启动记录与握手保存在工作区的 `.build/release-91e2c72/out/`。包内的 `metadata/managed/` 包含五项运行的原始日志。
