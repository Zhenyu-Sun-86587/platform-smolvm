# A07 冻结安装的 Hibernate 切点与内存回收

2026-11-26 在独立 KVM 工作目录运行两组 OCI 验收，分别保留零个或一个活后代。两组使用 fb69434 已原子安装的 Bundle `sha256:a9b41917173a0eea2306aa41276b59fa314e1d54f9d52f45766bcf2ce7698c35`。外部网络与 Worker 重启对照也使用该安装的运行时。[冻结输入与原安装记录](candidate-fb69434.md)

## 源机与停止边界

- Guest 每一 tick 检查并同步追加两个块设备的实际扇区，再经 virtio-console 向宿主输出 tick、PID 和两盘标记。两组均在真实 managed Standby 后进入 Hibernate，在确证已导出不可变切点时读取控制台和四个盘头摘要。源机经 pidfd 确认退出后读回相同内容，旧执行保持退出，重放原请求得到同一回执，提交后取消受到拒绝。
- 在两组成功休眠之前，同一 Worker 都曾遇到实际文件系统错误。夹具在每组独立目录中占用 chunk 目录路径，Worker 在 `exporting` 记录 `EEXIST` 且未发布 Manifest。调用公开 Native cancel 后，保存点进入 `aborted` 并释放 export roots，原执行 ID、epoch 和 PID 保留，原 Standby 回执继续生效。通过独立 Wake，Guest 从上一次可见的 tick 接续。
- 成功 Hibernate 后，持久根状态为 `available/durable/active`，临时导出根从 `available, 1 active root, 0 leases` 变成 `lost, 0 active roots, 0 leases`。移除原状态目录和控制 socket 后，从同一持久 Manifest 进行 exact Restore。两块写盘与只读根盘的卷身份均保留，Guest 保留原 PID 并紧接下一 tick。新 VMM 的 FD/VMA 中没有原 RAM inode。

## 外部 I/O 与重启对照

`packet-cut-native` 分别从 Running 和 Standby 发起 Hibernate。提交前获准的真实私网 SYN 令 Hibernate 回到原执行。提交后两个 SYN 共享一次精确恢复，服务返回所请求的数据，epoch 只增加一次。

同一安装的 Worker 与 libkrun 在原生两盘夹具中复测 `STOP_COMMIT`。重启 Worker 后，原进程保持原 Guest 计数并退出，已提交回执稳定。缺 chunk 与改动原盘均被拒。恢复保留原 PID、RAM 和两块写盘，随后又验证了独立救援点。

## 两组实际分配

逻辑 RAM 来自持久 Manifest。实分配按宿主的挂起源、Worker 和 Serve 所见 RAM memfd 的设备和 inode 去重，对每个无目录链接的文件读取 `st_blocks × 512`。源退出后检查存活进程的 FD 和 VMA，确认各 inode 是否仍有引用。持久文件统计来自隔离暂存目录中的 Manifest 与 chunk 文件。单位均为字节。

| 后代 | 逻辑 RAM | 实分配 RAM | 末引用释放 | 活后代共享并保留 | 持久文件实分配 | 持久文件逻辑大小 |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| 无 | 1,107,689,472 | 180,129,792 | 180,129,792 | 0 | 8,970,240 | 8,618,140 |
| 一个 | 1,107,689,472 | 193,966,080 | 30,875,648 | 163,090,432 | 8,880,128 | 8,524,148 |

无后代组的原 Guest PID 444 从第 2 tick 休眠，恢复后第一个可见输出为第 3 tick。有活后代组的原 Guest PID 446 从第 19 tick 休眠，恢复后为第 20 tick。活后代在该次捕获与原 VMM 退出之间独立推进至第 57 tick，捕获前为第 35 tick。

活后代的共享 capture 和原 attached reader lease 保持有效。原 VMM 退出后，抽样的六个 Guest GPA 对应的宿主 VMA 均未驻留，记录保留各页的 inode 和 backing offset。后代第一次逐字节验证 96 MiB 预置数据后，六页均为 present。

## 复跑

静态编译 [hibernate-cut-guest.c](../../tests/fixtures/hibernate-cut-guest.c)，以 root 运行 [installed_hibernate_cut_kvm.py](../../tests/integration/installed_hibernate_cut_kvm.py)，传入 `--bundle`、`--smolvm`、`--guest` 和隔离的 `--work-dir`。用 `--with-child` 选择活后代组。Guest 夹具的 SHA-256 是 `73862597ef2e3e4eda30086e40c201ea6249ca5e9c4b41334d2318f82f2625dc`。

原始记录在 `.build/release-fb69434/`。私网对照为 `installed-a07-packet-cut-native.log`，SHA-256 是 `723fde3167b3a28119ce30497ba51000388f43ef37ef933c4f9d73246aee3d72`。Worker 重启对照为 `installed-a07-stop-worker-restart.log`，SHA-256 是 `0c11331efbce4ab26179fa9c20a3425cf8d903f6ecbf090a2f98143e22b7de0c`。

| 主场景 | 工作区原始记录 | SHA-256 |
| --- | --- | --- |
| 无后代 | `installed-a07-no-child.log` | `6f099608556abe688bc53ac10899deae47581fc98ce996dad180bb7d4367bb7e` |
| 一个活后代 | `installed-a07-live-child.log` | `1ff4f8918746a8dec689846c42c64cddf529971185659c66257e9d3095c19ade` |
