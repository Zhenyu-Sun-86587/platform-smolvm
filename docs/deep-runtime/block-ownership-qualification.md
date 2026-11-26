# A10 块盘持有与跨 Bundle 争用

2026-11-26 在 Linux x86_64 上直接运行 fb69434 冻结安装的 Worker。两次启动均从实际进程的 /proc/exe 及 Worker 握手核对二进制，与安装 manifest 的 SHA-256 0c6454b969bd304fb98668ff963b9dc4a2b3703acd4228ea9762b8cd326f7d9c 一致。两份测试 Bundle 各登记两个真实进程及其可执行文件摘要，经 Worker 的 PID 核验和 Unix socket SCM_RIGHTS 领取盘 FD。矩阵在私有临时目录运行，四个进程只执行文件和 Worker IPC。另有两份开发 Bundle 的实机 KVM 对照。[安装锁与 Bundle 摘要](candidate-fb69434.md)

| 场景 | 实测结果 |
| --- | --- |
| 同代 RW/RW | A 已持有 O_RDWR 写盘且 fsync 写入标记。A、B 各一执行并发请求同一卷头，均返回 E_VOLUME_WRITER_CONFLICT。失败操作下请求导出 writer 返回 E_NOT_FOUND，收到零个 FD。 |
| 同代 RW/RO | 可写代在发放 writer 前即拒绝普通只读租约。A 已持有 writer 时，A、B 两路普通只读均返回 E_VOLUME_WRITER_CONFLICT，收到零个 FD。 |
| 路径和 inode 别名 | 指向同一对象目录的符号链接无法登记，返回 E_HOST_IO。另一路真实硬链接与原盘的设备号和 inode 相同。一份未登记 OFD 经硬链接持锁时，Worker 拒绝原路径的写盘预留。原 writer 接管后，从硬链接另开的 OFD 加锁返回 EAGAIN。B 申请包含无关卷和硬链接别名的整组写盘时，预留全部失败，无关卷随后可独立取得真实 OFD 锁。 |
| Worker 重启与未知 writer | 新 broker 会话将原 writer 标为 unknown，B 的预留返回 E_RECOVERING。四个进程完成 PID 重接后，恢复围栏仍返回 E_RECOVERING。原进程交回带额外硬链接的 backing 得到 E_HOST_IO。移除额外硬链接后，从同一 inode 独立打开的 O_RDWR 描述符仍返回 E_FENCE_UNPROVEN。交回原来带锁的 OFD 才把 grant 恢复为 attached，Worker 回到 ready，B 继续被拒。 |
| 原 PID 退出与写权限交接 | 原进程退出后，测试子进程继承的 OFD 仍持有原 inode。Worker 报 E_FENCE_UNPROVEN，B 仍无法取得写权限。子进程退出并关闭最后一个原 OFD 后，Worker 确认原执行 exited。B 随后在同一 inode 取得唯一写权限，读回并替换原 fsync 标记。 |
| 封存代并发 RO | 围栏退出 B 后封存同一代。A、B 分别取得独立的 attached 租约，均为 block_ancestor 和 O_RDONLY，读到同一设备号 26、inode 16771 及相同的 next-authorized-writer 字节。两个 pwrite 均返回 EBADF，完整文件摘要保持为 0b6fdf6751129336fb8637c2aee3963a7496c919c8c820a012eb85c07532f3dc。重新请求写盘返回 E_STALE_REVISION。 |

两份开发 Runtime Bundle 的物理 KVM 记录分别确认两个 VMM PID 273407 和 273567 及其 cgroup scope。第二份 Bundle 通过自身 Serve 尝试挂接正在写入的产品卷，Worker 拒绝申请，原目录记录保持一致。两台 KVM 继续在同一 Worker 执行。原 Bundle 后续在 Worker 重启后恢复其保留的 checkpoint。[双 Bundle 实机夹具](../../tests/integration/bundle_history_kvm.py)

## 复现与证据

使用 smolvm 提交 acfdf99c2a0331ee48a873dc02fc6002ea84d070 的夹具。从工作区根目录运行，临时目录供 Worker 和四个进程独占。

~~~bash
python3 -B smolvm/crates/smolvm-resourced/tests/block-concurrency.py \
  .build/release-fb69434/out/installed/smolvm-resourced --work-dir /dev/shm
~~~

另用同一个安装 Worker 运行原有 test_alias_exclusion_and_recovery 和 test_managed_block_read_lease_recovery_and_physical_gc，两项均通过。矩阵运行后，冻结工具以原摘要再次校验安装目录。

| 工作区记录 | SHA-256 |
| --- | --- |
| .build/release-fb69434/installed-a10-block-matrix-final.log | faf045ddef1d982af57dc1c4494c4e73e64dfef931efb3a5fc8c64361cc1a9b4 |
| .build/release-fb69434/installed-a10-block-matrix-repeat.log | c96bee1b516d8649e77d9033b57b314df25a856fdf56c5bc9ffc12e9a20a8207 |
| .build/release-fb69434/installed-a10-worker-regression.log | bb4376d674cbb436e8296cf262910139c82294066114f723040ab5be71f33541 |
| .build/release-fb69434/installed-a10-post-verification.log | 43fe8abfe19a2b94a32f1fc80657d09ee7d0ee8af29c8186b1cdf77b6f7aaa72 |
| .build/wp23/tracked-coexistence-kvm.log | d98a9e343c714e4eb7a82ba0e5a54052df2eb398d92087a5506222901d3f561f |
