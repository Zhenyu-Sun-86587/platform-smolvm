# A11 与 A12 卷代际及块盘生命周期

WP-10 的受管块设备闭包包含 root、storage、overlay、cache 和 data。公开 System 启动以 root 和通过卷 API 预约的 data 启动。storage、overlay、cache 由同一 Serve 和 Worker 的原生启动清单指定。v1 挂接和解挂在确认 Stopped 后执行。在线插拔各有独立的 storage.block_hotplug 和 storage.block_hotunplug 能力声明。[存储与持久闭包](TECHNICAL_SPECIFICATION.md#7-volume块附件与持久闭包)

## A11 旧保存点与独立推进的持久卷

fb69434 原安装以公开 API 创建两个独立 RW 卷，并在 snap_f8f07e5a4003ebe2d08f5db4f108bcc5 保存 RAM 与两盘旧标记。原卷继续推进，两次写入的最新标记随原卷转挂到另一 KVM。源实例删除后，Worker 回收当前私有 root head，保存点的旧 root 仍由原引用保护。

公开 exact 返回 VOLUME_GENERATION_MISMATCH 和 effect=none。clone_conflicts 在新实例创建两个独立卷，卷 ID 为 volume-045915485bf9439ba140df773c23ae4a 和 volume-707f6b41b32a5dfb0288564bbc8aff6d。两个新写头各自指向捕获时的确切盘代际，Guest 读回旧 RAM 标记、checkpoint-left-abcdef 和 checkpoint-right-0123。目标写入 clone-only 后，原卷仍读到 live-latest-left-aaaa 和 live-latest-right-bbbb。

重启 platformd 后，公开 attachment ID 与 Worker 保存点回执逐项一致，新卷的公开 writable head 与实际 VMM 绑定一致。两个实例的 RO attachment 共享封存代 op_43dad661b9bff09b2f39749f6142d852-head。目标 Hibernate 时旧拓扑变更被拒，随后 exact Wake 启动下一 epoch，保留两个公开卷及 clone-only 字节。

最终复测使用 4 GiB 专用临时文件系统。首次共享临时目录可用额度不足，正常准入在创建克隆前以 RESOURCE_LIMIT 结束。成功记录 .build/release-fb69434/installed-a11-volume-projection.log 的 SHA-256 为 08f9ea4a324a6d72d81a0139fe5824d9bfe489b56ecbeffe0f0a9e0e0822bfab。额度拒绝记录为 .build/release-fb69434/installed-a11-insufficient-shared-scratch.log，SHA-256 为 d4cd754449c54d4b40d4c99892a4cff0d09e46dbd3c22bba566bc87069257112。

开发 Worker 和 Platform 另对一块 machine 卷及一块 persistent 卷完成同样的旧点恢复。新 machine 卷的公开归属为目标 Worker 实例。目标 Hibernate 期间，三路拓扑修改保留 VOLUME_TOPOLOGY_UNAVAILABLE 并携带 REQUIRES_STOPPED。下一 epoch 继续读到克隆字节。原始记录 .build/release-fb69434/development-a11-machine-checkpoint.log 的 SHA-256 为 177d2099013b40f01c5406c697740c4abcbaf3603c07bf61915ac5840da270a5。

f2df146 测量组合在真实 KVM 上再次完成源卷推进和转挂、源删除、exact 拒绝、clone_conflicts、平台重启、公开卷查询及下一 epoch 精确唤醒。两个目标卷为 volume-526c0ab44010c9fb9f4d8e4d02e15c15 和 volume-14825b269b38e65e0d9181e95177dc67，RO 封存代为 op_0cd48ce2d51d388ba596fb78ace95e1b-head。该组合使用 f2df146 发行输入的 Worker、Serve、platformd 和 Debian 盘，测量 manifest 摘要为 sha256:1d79bf8139035f924f8132fe58837686d6da9a0c72bc2c3ee2f32829882bedff。截至此次运行，f2df146 已安装目录的授权范围为 OCI。System 安装资格由后续版本化证据决定。原始记录 .build/release-f2df146/measurement-a11-volume-projection.log 的 SHA-256 为 6d181970d50ff43f35f13b454728cf538bcfc57c896174e2c55835296fd2ee53。

## A12 同一保存点内的五种盘角色

fb69434 冻结安装在一台真实 Debian System Guest 上同时启动五块盘。Guest 读回各设备的只读标志，向 root 文件系统及 storage、overlay、cache 分别 fsync 独立标记，并读取 data 的固定只读字节。随后原生保存点同时记录 RAM 和五种盘角色。源在保存后继续写入四块可写盘，并经原生 Stop 确认退出。目标按原保存点启动，恢复前序四盘字节和 RAM 标记。四个后继写头独立创建并指向下表的确切封存代。data 使用相同的不可变代际，Guest 继续读到原字节。

| 盘角色 | 保存点代际 | 目标 Guest 访问 |
| --- | --- | --- |
| root | head-6de0966527cd23268a01a452952f9c92 | RW，读到 original-root |
| storage | head-f3b50847e7666e9fe2390a80057d4b40 | RW，读到 original-storage |
| overlay | head-dca00e6a811efbc0f92ba237007fe017 | RW，读到 original-overlay |
| cache | head-b4b56e11f5bd97b9bb3a8d9f94afc2a9 | RW，读到 original-cache |
| data | fixture-all-role-base | RO，读到 immutable-data-device |

保存点为 point-eb2f8732436d722d1c64135afb76737c。清单摘要为 sha256:942e4e69de18280d9fb9119773c12fb90a87c23c99f55c6a24d300846a8634f8。原始输出为 .build/release-fb69434/installed-a12-native-roles.log，SHA-256 为 58b3d3ec443f91aba47b734f395981a9948909a5207375e3ba89ba056f515086。[冻结安装锁](candidate-fb69434.md)

## A12 公开挂接、解挂与扩容阶段

fb69434 以真实 System Guest 运行公开块盘操作。在缺少在线插拔能力的配置下，Running 的挂接与解挂均以 PRECONDITION_FAILED 结束，在线增长为 CONFLICT，Guest 块设备和原数据保持一致。确认原 PID 退出后，持久卷增加到 32 MiB。冷启后 Guest 读到已增长的块容量，同时文件系统仍为 16,777,216 字节。夹具将 Guest 块设为只读后实际运行 resize2fs，失败后公开块容量和旧文件系统大小保持一致。恢复 Guest 写权限后，resize2fs 完成，读回原文件。

原持久卷转挂到另一真实 KVM，读回 portable-original-bytes。源机与另一 KVM 同时通过两个独立 RO 租约读取同一个封存盘。源机完成公开删除后，私有 root head-d7556677c5d44b365d93393a74122a19 从资源目录和磁盘收回。旧 ephemeral 及持久卷仍可用，另一 KVM 继续读取同一 archive 代际。原始输出为 .build/release-fb69434/installed-a12-volume-lifecycle.log，SHA-256 为 2432be6d51f801ab46b2655af4dbc2a0b6b527df418d184f7a0a90eec8956c69。

## A12 实际存储错误与提交切点

fb69434 Worker 在测试私有 ext4 对象盘上遇到真实 ENOSPC。公开 Create/Grow 返回 VOLUME_STORAGE_NOT_APPLIED 和 effect=none，原卷仍为 16 MiB，原 inode 15、修订和字节摘要均保留。释放测试填充空间后，真实 KVM 冷启读到相同的 16 MiB 摘要 acdfd1236350ce9f7b16f0a5c04cbb73606edbd268d60a3058c686f54e7eaa7a。

替换盘已暂存并同步后，夹具只在选定旧 head 变为 0400 时注入 fsync EIO。操作返回 recovery_required 和 effect=unknown。旧 head、原修订及 16 MiB 容量仍为公开观察，竞争卷快照请求无效果结束，未新增快照。重启 Worker 后，公开 reconcile 沿用同一个 operation ID 和替换 inode 19，卷修订只增加一次。新 Guest 在 32 MiB 后继盘上读回原 16 MiB 并成功继续写入。

原始输出为 .build/release-fb69434/installed-a12-storage-cut.log，SHA-256 为 26a4a137a5eeade54dac9daf443a22001945aa43b306b0b0a156b8be28a50060。

## A12 私有 machine 卷与旧目录

显式 machine 卷首次可写挂接时记录稳定的 Worker 实例。后续解挂、Stop 和冷启动保留所有权。独立持久恢复时，新写卷归目标实例。计算退役经同一挂接与写权限围栏封存其私有写头，实际 GC 继续核对保存点、卷快照和其他仍活引用。

开发组合 sha256:db5c7dda538b1a3b62cd450365058e15b9bc1d782c3934ae230593f99977a4dd 使用 fb69434 的 Serve、libkrun 和 Debian 镜像，另使用 schema 29 Worker 与更新后的 platformd。Running 的公开挂接、解挂和增长均带 REQUIRES_STOPPED 结构化原因，外层仍为冻结对照观察到的 PRECONDITION_FAILED 和 CONFLICT。

该组合先在原机写入 machine-private-bytes，再取得固定快照。原机删除后，写头 op_c8290cb68308d26f1372feef6ac7697e-head 完成物理回收。同 owner 的两个只读消费者仍可使用 op_1a26c1f964b35826692eb1f5cd78aaf9-head。第二个消费者在原机删除后冷启，两个 Guest 均通过真实块设备读到原标记。第一个消费者 Stop 并完成公开解挂后，第二个消费者仍可读。原始记录 .build/release-fb69434/development-a12-lifecycle-final.log 的 SHA-256 为 245b9e83464ace6125d81aeb83fc77a7b94e4f803090bb6fff90826f9ecca852。

真实 Worker IPC 回归使用 schema 29。原机器退役后，65 个已挂接或之前解挂的私有写头全部物理回收，保留有快照 pin 的封存代。只读快照挂接保持原归属记录。同一目录的 legacy ephemeral 卷保持可用及可转挂。另一组使用 f2df146 schema 28 的 Worker 先实际创建和挂接 legacy ephemeral 卷，再用 schema 29 Worker 打开同一目录。升级保留原租约和 inode。随后退役原机器，仅解除旧挂接，原卷仍可用。

| Worker 工作区记录 | SHA-256 |
| --- | --- |
| .build/a12-worker-products-final.log | a35b20d3532e81678b45b64db96d67ecaabe86a8f712410c936d1beac00dbc94 |
| .build/a12-worker-upgrade.log | 670d9b99463b6c1cb59cc934609195cdee627a13b39ba5abe5ee2eb401aee743 |
| .build/a12-worker-final-tests.log | 8d9d9ee07a9fce55707f63dd6b956a4978746b44f9ad72109135f2bba1369f73 |
| .build/a12-final-development-inputs.log | 99818eab86b8105d73d6fc057e0a1e400f984d1846b540927afdd04c46d36417 |

## A12 独立 Fork 的公开磁盘

同 owner 的两台真实 KVM 使用开发 Bundle sha256:a4f8cc098660ccf58c3e8b632dd4a6b2f73bb235cd73a2745428a711e89910de。公开独立 Fork 发布两个新写卷和一条独立的 RO attachment，实际 capture 为 ram-9355bca4290fd8036f38465b5e12a430。父子写入各自标记，两侧都读回自己的字节。

| 生命周期或访问 | 父卷 | 子卷或共享代际 |
| --- | --- | --- |
| machine RW | vol_3fe8bcf171e8dd9daddda9fe689e4723 | volume-1556a59c471debc73c687e48f3ee69a5 |
| persistent RW | vol_5e2245d7c6e81e6983ab3e8e3f6b63fa | volume-d83973c9d5f0c2a6b8babf383acff46c |
| immutable RO | 同一 archive，独立 attachment | op_d39b4dfad2bfedf7210e8a1a0d4f3330-head |

父机完成删除后，父 machine 写头 next-431f3c8805575408fc42d0f241945dc3 回收。子机两次从 Stopped 冷启到 epoch 2、3，保持自己的 root head-1d95eb4258987377e755f0155a5d68a0、根文件、两块写盘及共享只读字节。子机随后删除，回收自己的 machine 写头 head-40a1f8927aab45f868a7fc152d5691ee。两个独立 persistent 卷均保持 available，archive 快照仍可查。

开发对照的首次子机冷启暴露了捕获 root 与常规模板使用不同 attachment ID 和代际的问题。修复后，Worker 根据原 System base 验证并复用受管子机已绑定的确切 root slot 与写头。以下为带输入摘要、原问题和修复后结果的工作区记录。

| 工作区记录 | SHA-256 |
| --- | --- |
| .build/a12-fork-child-cold-diagnosis.log | f11932e4b84ed0d7902c53f3cf3ceb703d1bdfc8d35a306670e790f446f5b51a |
| .build/a12-fork-cold-development-inputs.log | b89c18451591b68b33a60ca015d19db09ec1b60fa1c53b6828918c549a564ba1 |
| .build/release-fb69434/development-a12-all-fork-volumes-cold.log | 17e385920d0b843724a6823f43cf89d759aff4a98740a867385485743b37b68a |
| .build/a12-platform-fork-cold-tests.log | fd3aa51803d0fcd8aa479133f6acc6bcff21ec37ea6fc3dab5020ea86ad6ff3c |
| .build/release-fb69434/installed-a12-post-verification.log | 43fe8abfe19a2b94a32f1fc80657d09ee7d0ee8af29c8186b1cdf77b6f7aaa72 |

## 复现入口

卷夹具在本机私有 mount 和网络命名空间运行。按案例使用 native-roles、lifecycle、growth-fault、deleted-checkpoint 或 machine-fork，与对应 Bundle、platformd 和 Debian 镜像的绝对路径一起传入。--work-dir 指向专用临时目录，已安装组合用 --use-installation 对原目录做只读绑定。machine 及结构化原因断言用 --machine-volumes。growth-fault 使用 volume-fsync-fault.c 生成的测试专用 shim。运行范围为同 owner 自有测试卷及 KVM 的正常操作，越权和伪造授权用例未执行，且不计入结果。[KVM 夹具](../../tests/integration/volume_product_kvm.py) · [定点 fsync 夹具](../../tests/fixtures/volume-fsync-fault.c)

Worker 夹具为 smolvm 的 crates/smolvm-resourced/tests/product-volumes.py。传入单个 Worker 跑 product volumes，传入 --upgrade、原 schema 28 Worker 和新 Worker 跑同目录升级。
