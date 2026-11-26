# 冻结候选 d8cc5d5 的安装与 KVM 验收

2026-11-26 在隔离的 Linux x86_64 Worker 上安装了 Bundle `sha256:c5070578cdf0d4fd01cc120721515c169e9297a3f624803def398996c0e249b5`。从安装目录启动 Serve 和特权 Worker，握手报告了该 Bundle、完整 StatePoint reader 和已测能力。

| 输入 | 固定值 |
| --- | --- |
| smolvm | `d8cc5d5a3585f3570bc071a87eb0de767a3060b7` |
| libkrun | `b77e9d3a62bf3b6d6ba0245cb7b9e6c536772a97` |
| libkrunfw | `0cbb229e7b55698028e78fef9afe9c32797d0b3f` |
| 平台验收工具 | `9eb18ae198be3ab361d1222f7d5c5a8beb70493d` |
| Debian System 盘 | 805306368 字节，`sha256:ad9afc654e73f8640ba5859f2079dc18c66ab4fa73d64c4217338babb1860d7b` |
| 包内测量记录 | `metadata/managed-acceptance.json`，`sha256:9c8856fa12867575d8e3f6b28e7bc4262c42f058fa9f4c3be471281e8ea820c0` |

包内七份 KVM 日志覆盖 OCI 双盘状态捕获、System relay、System Fork、System 连续捕获、两级自动休眠，以及 Native 到 FixedInternal 和反向的私网唤醒。两路网络测量使用相同的测量包 `sha256:f1295d42046808e3eba3abec92cedb4838d7d0314cc88e531a0f02da22df63cc`，各自验证 TCP、Standby、持久休眠恢复和有界 UDP 交付。七份日志逐项重算的摘要均匹配发布记录。

| 安装后验收 | 实测结果 |
| --- | --- |
| OCI 启动 | 构建目录和安装目录各启动两个 OCI Guest。启动证据逐字节相同，SHA-256 为 `bf27ae8b2a0e69feb111b49a71884240851df78a8e423d3718df69515ae52731`。 |
| Serve 和 Worker | 两个实际运行的 ELF 摘要均匹配安装文件。握手确认 System 启动与恢复、Standby 与 Wake，以及 Native、FixedInternal、TAP 和 TAP restore。单列的 Native 身份重建能力仍报告 `unknown`。 |
| 公共 Guest 连接恢复 | 在 Debian Guest 连接存活时强制结束 platformd。重启后仅旧进程的连接保护被释放，独立运行租约保留，延迟到达的旧申请被拒。 |

工作区记录位于 `.build/release-d8cc5d5/`。其中 `out/installed-handshake.json` 的 SHA-256 为 `7a9364d07c6ce7e558b122ca543ef03acb97f2f9fde70bea099ec4457e51ec45`，`installed-connection-kvm.log` 为 `855d86a000d2e53681bd7f38b5f7eaa62cee952edf99d430190a7fcfb61ab126`。各工作包余下的组合验收见[完整台账](acceptance-ledger.md)。
