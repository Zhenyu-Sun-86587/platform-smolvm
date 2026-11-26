# 冻结候选 fdd620e 的安装与 KVM 验收

2026-11-26 在隔离的 Linux x86_64 Worker 上安装了 `sha256:1c77726df6c3936ce81a2fcb7cbee5787501fc9e078075f2a0e2d17f0008783a`。构建目录和安装目录各启动两台 OCI Guest。两份 OCI 测量逐字节相同，安装后的 Serve 也返回了该摘要和 manifest 声明的 StatePoint reader。

| 构建与验收输入 | 固定值 |
| --- | --- |
| smolvm | `fdd620ecc2a13ef492dcd310515e18cda9abbefe` |
| libkrun | `b77e9d3a62bf3b6d6ba0245cb7b9e6c536772a97` |
| libkrunfw | `0cbb229e7b55698028e78fef9afe9c32797d0b3f` |
| 平台验收工具 | `35c923ef4afcac6c79ccdb0051ab2edf290ec30a` |
| Debian System 盘 | 805306368 字节，`sha256:ad9afc654e73f8640ba5859f2079dc18c66ab4fa73d64c4217338babb1860d7b` |
| 包内五项真机测量 | `metadata/managed-acceptance.json`，`sha256:61f18544ca2bac5c14007c8dbbfb1841848a937d65157846f290c5570ff91d81` |

五份随包日志覆盖 OCI 双盘捕获与恢复、System vsock、System 分叉、连续 System 捕获以及两级自动休眠。逐项重算的 SHA-256 均匹配已安装元数据。

| 安装后 Debian 场景 | 观测 |
| --- | --- |
| Native 和 FixedInternal 私网 | 两个方向分别发送 TCP SYN 唤醒 Standby 和 Hibernate。持久恢复保留私网地址并更新执行 epoch。一次授权的有界 UDP 数据报送达，大包触发唤醒后被丢弃。关闭策略或使用未授权的端口、传输协议时，虚机保持 Standby。 |
| 运行保护与主动分叉 | 持有运行租约并设置常驻的 Guest 在用户进程等待时分叉。两个子机延续原 PID 和 RAM nonce，回复独立，各自继承策略。 |
| platformd 崩溃恢复 | 强制结束 platformd 后，新进程回收前一个进程持有的 Guest 连接保护，独立运行租约保留。迟到的同一连接申请被围栏拒绝。 |

安装后的四次 Debian 验收共用一份原始记录，工作区路径为 `.build/release-fdd620e/installed-system-kvm.log`，SHA-256 为 `239bca5c53a10c612d18fb298e8fe5741e6eb662ec45a269bfb4b4f3ec705d22`。同目录的 `out/installed-handshake.json` 记录了独立 Serve 握手。此候选沿用 v1 发布能力声明，后续网络恢复授权、压力准入及控制台改动的验收范围见[逐项台账](acceptance-ledger.md)。
