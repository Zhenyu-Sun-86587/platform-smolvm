# A09 冻结安装的两盘事务切点

KVM 用例使用 fb69434 已安装 Bundle `sha256:a9b41917173a0eea2306aa41276b59fa314e1d54f9d52f45766bcf2ce7698c35`。Guest 将事务编号和相位存入同一个原子 RAM 字，在两块真实块盘的固定扇区分别写入带盘身份的计数。每次写入执行 fsync，读回检查整个扇区。奇偶事务交换先写的盘。[冻结输入与安装记录](candidate-fb69434.md)

| RAM 相位 | 两盘合法计数 |
| --- | --- |
| 准备 | 两盘均保留上一事务。 |
| 第一盘写入中 | 第一盘可为上一事务或当前事务，第二盘保留上一事务。 |
| 第一盘完成 | 第一盘为当前事务，第二盘保留上一事务。 |
| 第二盘写入中 | 第一盘为当前事务，第二盘可为上一事务或当前事务。 |
| 提交完成 | 两盘均为当前事务。 |

源持续执行事务。夹具按种子打乱六种切点，加入随机等待后执行 managed Standby，从同一执行进行实际 Fork。另做两次只等待种子指定间隔的盲切。每个目标在首次发现自己的新 Guest hostname 时，先读取继承的 RAM 相位及两个盘扇区，再继续原事务。夹具比较源机在切点的实际读数，并核对每次都完成了包含两块盘的 WriterPivot。写入阶段等待磁盘 fsync 完成后的两个测试窗口允许盘先于 RAM 相位推进。

## 真实切点和恢复结果

种子为 92077。八次 Fork 各有独立 capture ID，目标均延续原 Guest PID 447。以下读数在源切点和目标首次读取时一致。

| 采样 | RAM 事务 | RAM 相位 | 左盘 | 右盘 |
| --- | ---: | --- | ---: | ---: |
| 定向 | 1 | 第一盘写入中，右盘已完成 fsync | 0 | 1 |
| 定向 | 2 | 准备 | 1 | 1 |
| 定向 | 2 | 提交完成 | 2 | 2 |
| 定向 | 3 | 右盘先写完成 | 2 | 3 |
| 定向 | 4 | 左盘先写完成 | 4 | 3 |
| 定向 | 4 | 第二盘写入中，左、右盘均完成 fsync | 4 | 4 |
| 盲切 | 4 | 提交完成 | 4 | 4 |
| 盲切 | 5 | 第一盘写入中，右盘已完成 fsync | 4 | 5 |

另从事务 6 左盘先完成的切点执行 Hibernate。exact Restore 仍使用 PID 447 和原卷身份，首个相位样本为 `split`，盘计数为 `(6,5)`。随后同一进程实际提交了事务 6，两盘读回 `(6,6)`。

静态编译 C 夹具，以 root 运行 Python 夹具并传入 `--bundle`、`--smolvm`、`--guest` 和隔离的 `--work-dir`。Guest SHA-256 为 `bfc8b985920f58f4ecc06e8789a0f855ed339d7aee1c4c03b656bb528a1c7379`。原始 JSON 在 `.build/release-fb69434/installed-a09-transaction-phases.log`，SHA-256 为 `9ba0469a93b546d9933697465efe1de81b96a0bba76a71fcb7c49f4908cdcf9b`。[C 夹具](../../tests/fixtures/multidisk-phase-guest.c) · [KVM 运行入口](../../tests/integration/installed_multidisk_phase_kvm.py)

## 两个真实 capture 的混搭负例

在已安装 Worker 和 libkrun 的两盘夹具中，实际捕获源和递归子机，分别得到 `source-pivot` 的 `source-head-0/1` 和 `child-pivot` 的 `child-head-0/1`。新目标通过 Worker 取得独立写头，再用 `source-head-0` 与 `child-head-1` 分别搭配两份原始 RAM capture 试图恢复。两套绑定都返回 `replacement disk does not extend the exact captured block generation`，目标控制 socket 与 Guest 输出均未出现。原来正确的递归对照仍恢复且继续读写。

负例夹具变更在 smolvm `b3e3bc543eb4818520f5db472c65d7b6f9ecb7e2`。用该版测试脚本运行 `kvm-parked-block.py --mixed-capture`，传入冻结 Worker、libkrun、静态块盘计数器和 identity-helper。工作区记录 `.build/release-fb69434/installed-a09-mixed-capture.log` 的 SHA-256 为 `3762c468dc390b44f2bb8f44a187e0a19595478b709ae9394542d8235aa87caa`。
