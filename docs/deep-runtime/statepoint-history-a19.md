# A19 历史 StatePoint 恢复

试验在私有挂载和网络命名空间、专用 systemd slice 中执行。两路安装只读绑定，由同一个 Worker 承载。`sandboxctl` 使用本试验 owner 正常签发的管理令牌，调用公开创建、捕获、查询和恢复。平台使用冻结 `c011476`，实测二进制摘要为 `sha256:0b9b27678863d8bf6ecfb7df6409b2765112dc92dc88a3a7da6db853e01948c6`。CLI 摘要为 `sha256:12da76f0be8953d2c3cba0ca8fe6e9ec6e949fef80358b41193d9481d60cea0b`。

## 两个历史执行点

旧 Serve 来自 `d8cc5d5`，Bundle `sha256:c5070578cdf0d4fd01cc120721515c169e9297a3f624803def398996c0e249b5`。新 Serve 与共享 Worker 来自 `fb69434`，Bundle `sha256:a9b41917173a0eea2306aa41276b59fa314e1d54f9d52f45766bcf2ce7698c35`，Worker 摘要为 `sha256:0c6454b969bd304fb98668ff963b9dc4a2b3703acd4228ea9762b8cd326f7d9c`。两个独立 KVM 进程同时运行，旧机为 1 vCPU、512 MiB 的 FixedInternal System，新机为 2 vCPU、640 MiB 的 System。

旧机上的服务保持一个实际 Shell 进程，并持续递增 RAM 计数器。在不同的两次公开捕获前，分别写入 `/run` 标记和根盘标记。停止旧 Serve、移除其运行配置后，首个快照仍然 `ready`。恢复请求返回 `CAPABILITY_UNKNOWN`、`effect=none`，Worker 的执行清单与新机的执行引用相同。Bundle 引用工具从平台 schema 1019 和 Worker schema 27 读到原 reader 的两个持久引用，返回 `can_retire=false`。

重新注册旧 Serve 后，两次公开 Restore 分别从原 manifest 恢复至新的绑定和 epoch 1，保留原 FixedInternal 网络配置。Guest 的实际任务 PID 514、启动时间 303 两次相同，各自读回对应的 RAM 与磁盘标记，计数随后继续递增。旧点 manifest `sha256:f120e20ada12e2596640f58724755d2c85aa7b7444ec2060f0a441a7f194886c` 的计数从 7 到 8，新一点 `sha256:fadcfa2d3e8b96f4f9eb6098b02885c3f8af3d0e3e162a16bd37a21e98a13d74` 从 17 到 18。`.build/a19-history-public-v6.log` 的 SHA-256 为 `6d5b8002570bf18d11515f64f22c2c4952728ead048a2f6cf0de1d69e4e746b3`。[公开恢复夹具](../../tests/integration/statepoint_history_kvm.py)

```sh
sudo python3 -B tests/integration/statepoint_history_kvm.py \
  --bundle "$d8cc5d5_installed" --current "$fb69434_installed" \
  --platformd "$pinned_platformd" --sandboxctl "$sandboxctl" \
  --system-image "$pinned_debian_disk"
```

## 归档副本的常规完整性检查

冻结 `fb69434` 的公开 `backups run` 对已登记的合法 reader ID 返回 `NATIVE_CLOSURE_UNAVAILABLE`。Worker 的旧选择校验将实际包含冒号的 reader ID 按普通资源 ID 校验。`.build/a19-history-public-v5.log` 的 SHA-256 为 `3453a78457c617a0d90e9cd658d961a26f65e226b0960f9131558cb23996c45f`。

开发 Worker `sha256:d81a5b2f5ad8d322bacc641a1a330b54db77085bd691d2bf317e0cac91c515cd` 按登记的原 reader 合同导出两个完整点的只读描述符。其 `recover-backup-closure` 校验 manifest `sha256:413bd9b412a1b0838270ad790052eda5f99223063b646ccc1940206c2a265feb`，再验证独立副本缺失一个普通 StatePoint chunk，以及该副本内同一 chunk 截断一字节。两次副本校验均失败，完整归档再次通过。旧 Serve 随后恢复两个点，Guest 任务 PID 610、启动时间 492，分别从 16 到 17 和 69 到 70。`.build/a19-history-copied-archive-v1.log` 的 SHA-256 为 `9447f727979112009318d5b757e28495529f42c89d7aefe80ebcc1fc1f1bebac`。由前述夹具增加 `--worker <binary> --verify-backup-copy` 复跑。

## 公开备份与独立 Worker 恢复

含 reader 修复的开发 Worker 来自 `1eb5104`，二进制摘要 `sha256:024d00af15399915a5ae399a495fae9448c99af73f846f625a59451ee9f52e0a`。开发 Bundle `sha256:0b9e399f082e7b021a4cd078711b5bcd46843bdbb8c08b1f6f4bae923e4232b5` 使用 `f2df146` 的 Serve、Agent、库和固件，夹具单独复制后只读绑定。使用平台 `sha256:7a1c13411b276606a17c1564c4279f4117fd5f403f45f30d8d6582458bc36725`、CLI `sha256:462a66b2b53d14553c607b378d6ef6f328300621e021cef4eead780a20f11ca7` 及 System 盘 `sha256:4b5ba5301eac04a398f50fb5be2f969c6e793845d249303783dc2f69a3a3c23f`。

所有者使用公开 `sandboxctl snapshots capture` 保存运行中的 Debian System，再用 `backups run` 创建归档。运行达到 `verified`，管理员的 `backup verify` 核对原登记 reader `managed-acceptance:sha256:c41d1ef5a0e49afd56b8ef272e8355c588030ebcb68041e6d62d1942945cf01c` 和闭包 manifest `sha256:59e924e4de0a1a4bc8c9a8184340ce1841a34036955be991ecdc2e1dc7a13d50`。

`backup recover-native --worker-catalog` 从空目标目录生成独立 catalog，初始没有活执行。相同 Worker 与 Serve 在新目录从原 StatePoint manifest `sha256:3cf136c9f7af0b64f290a67dcadfbd430aa47bb5f7cf658d0105a9a25ef3cf44` 启动新的 KVM 实例，目标为 epoch 1，原 `/run` RAM 标记与根盘 `/var/tmp` 标记均读回。源执行在全流程中继续使用原引用和 epoch。`.build/a19-development-public-backup-v2.log` 的 SHA-256 为 `3fbaeec076e289d1b83067b11fd272ddebccef49b64ba8ff1185a3762ffbfc51`。[公开备份夹具](../../tests/integration/authorized_statepoint_backup_kvm.py)

```sh
sudo python3 -B tests/integration/authorized_statepoint_backup_kvm.py \
  --bundle "$reader_bundle" --platformd "$platformd" \
  --sandboxctl "$sandboxctl" --system-image "$system_disk"
```

### 冻结安装复测

以只读挂载复跑正式安装 `1eb5104`、Bundle `sha256:b00c0f05bc54d69bbef854ea16c3cac42d36ddc9f8413a1cedd5e5e290434f97`，使用锁定平台 `c095c4f`。运行的 Worker 为 `sha256:f9a0812af46952abdd86cb99f596862e8f9f097c3e84dbebbe92cac50ad23448`。公开备份达到 `verified`，管理员核对安装件的原 reader `libkrun-portable-snapshot-v1:1eb5104630b01fe5e21494f979579f3fa9924427:0d0255383f4378aca5be7bf19629330c08fcaf8e` 与闭包 `sha256:8b1b29b077db4701eb855bb0fb2246fb8552dba2a4ea759752cfb0a73f6cf6d8`。

同一安装件在空的独立 Worker catalog 从原 StatePoint manifest `sha256:d6342e495f1dccea3a0bcb9f179d8d585f326c4cc4797de2b8a91b66b38dcdea` 启动新的 epoch 1。目标 Guest 读回 RAM 和根盘标记 `a19-new-reader-78edc1f3c74bfda137ae6860`，原源执行保持原引用。`.build/a19-1eb5104-installed-public-backup-v1.log` 的 SHA-256 为 `08677470f3dccb5715530a911d9ed318a4ca229a419f78eccc4f915b7f803300`。

## 便携增量与恢复资源

冻结 `fb69434` 的库、固件和 OCI 根文件系统配合 A17 的开发 CLI `2f39d93`、Guest Agent `b09a5c0`。公开 `machine checkpoint --store` 创建两代，第二次写入 7 MiB 并复用 127 MiB。`--export-from --history 1` 发行携带两代的普通归档 `sha256:ef4c4c39047dd21db1e8f0e9fedda342cad2183b7ebf8364275896c223bcbca4`。用 `--at '~1'` 和 `--at '~0'` 分别启动两个目标，读回各自 RAM 标记、原 crun PID 480 与启动时间 97，计数分别为 14 和 27。显式请求 2 vCPU 或 768 MiB 时，CLI 分别指出原值为 1 vCPU、384 MiB，并拒绝创建。`.build/a19-portable-history-v2.log` 的 SHA-256 为 `0e36d9492d7ca05bf2f1efb41313dd9eb4136162b66f314d742438a691874f83`。[OCI 夹具](../../tests/integration/oci_agent_restart_kvm.py)

在另一台真实 KVM Guest 中，经独立只读挂载读取同一个归档 `sha256:d475053725c2f941a9e66135a2a2aa069de31b51fb46cd62109719e2979ccfaa`，Guest 实算文件 SHA-256 一致。AMD 宿主与该 Guest 的 `/proc/cpuinfo` flags 摘要分别为 `eb38fc86c301e99a8ca9db934cbfddc9d302fdaab0053a29f244888541b757fd` 和 `841a86f4bbad2596e5db45fc5cd198697cb643adfcca6611ddac52c197aabbe3`。同一开发 CLI 在 Guest 内返回 `checkpoint CPU feature contract does not match this host`，机器清单没有目标条目。宿主用同一归档的两代各启动了真实 KVM 目标。`.build/a19-portable-history-cpu-v2.log` 的 SHA-256 为 `045be931a15d21925f567c00d696b42d99eee42ad03e80d281a9d7afe2f8c483`。

```sh
sudo python3 -B tests/integration/oci_agent_restart_kvm.py \
  --bundle "$fb69434_installed" --agent "$development_agent" \
  --smolvm-bin "$development_smolvm" --end history --verify-guest-cpu-contract
```

## 小型历史语料索引

下表沿用 Linux x86_64 KVM 上使用的发布工件和私有夹具。每个样本由原 reader 与生成命令重新形成完整闭包，输入只包含夹具自己的写盘字节、计数或标记。[包内九项测量和 reader 工件](candidate-fb69434.md#九项包内测量)

| 样本 | Bundle、CPU 和配置 | 同一 reader 的复跑入口与结果 |
| --- | --- | --- |
| OCI 原生保存点 | `fb69434` 包内测量 `sha256:7681ac8c72c819d931c05d721e490ede26db61131bd698c8940a6ecb7fc83884`，Linux x86_64，OCI | `measure-managed.py` 的 `oci-capture`，记录独立双子、两盘与 RAM、Hibernate、exact Restore。包内日志 SHA-256 `555cd8fd3c0f0a3f82d7ed9a1ea993048676cbd1b39755720a13cae6f78e5876`。 |
| System Native | 同一 `fb69434` 包内测量，Linux x86_64，Debian System，Native | `measure-managed.py --private-network` 的 `packet-native`，同私网地址及时序，持久点唤醒接回实际服务。包内日志 SHA-256 `8fdafd5b1141e5a7097c35f92fd35b9058d771f58fa2162e4a8a851049b72da0`。 |
| System FixedInternal 与活任务 | 本页 `d8cc5d5` 原 reader，1 vCPU、512 MiB，Linux x86_64 | `statepoint_history_kvm.py`，重新恢复两个保存点后原任务 PID、启动时间、RAM、根盘标记和计数均验证。 |
| 附加盘 | `d8cc5d5` 的 Linux x86_64 System，有两块独立可写盘与一个只读代际 | `volume_product_kvm.py --case checkpoint --checkpoint-data-only`，原卷推进后 `exact` 返回代际冲突，`clone_conflicts` 恢复各盘在切点的内容。[原始卷实测](acceptance-ledger.md#卷恢复与停机存储实测) |
| CPU offline slots | 开发 Bundle `sha256:b79f24bd67af481dbe80c5ed9d870848b012d523107adb78a309e62043bb99b9`，Linux x86_64 System，两个已建槽 | `managed_system_vsock.py --case live-resources`，下线一个 CPU，Hibernate 恢复后保留在线集合，在新 epoch 上线原槽。`.build/native-resize-integrated-kvm-v2.log` SHA-256 `f1dd18d5b43e2d64abc68ed8b8244797adb2ef51ef3a5420e18f9060335aadf9`。[资源恢复与边界](native-power.md#wp-21-运行中调整-cpu内存和-balloon) |
| RAM growth | 同一开发 Bundle 的 Linux x86_64 System `live-compute`，1 vCPU/512 MiB 到 2 vCPU/640 MiB | `managed_system_vsock.py --case live-resources-hot`，实际新增 134217728 字节。此配置的完整捕获按当前能力拒绝，原机继续可用。`.build/wp23/public-resize-kvm-v3.log` SHA-256 `8fa52011113e17dcd90ca3aa4b5c787ee64ea3bdecbcb59df68ed86b106a7739`。 |
| 长 Standby | 时钟修复开发 Bundle `sha256:460a09b6b40081ed77aa3268d1fb6e0e496cbba128bf4fd3d6e9863e9599896f`，Linux x86_64 Debian System | `standby_clock_kvm.py`，两组 managed 24 秒，先后 Fork/Hibernate。两个恢复目标继续原 Guest 定时进程，均约 9.008 Guest 秒到期。`.build/a08-held-clock-kvm-v2.log` SHA-256 `c76456893b9fd25f403d75a0fc0caccbb2031bbf86fbcd2f2757f1043cd9e82f`。[长待机记录](acceptance-ledger.md#a08-长待机和保存点时钟) |
| 便携增量 | 本页 `fb69434` 库与固件，开发 CLI `2f39d93`，1 vCPU、384 MiB，Linux x86_64 OCI | `oci_agent_restart_kvm.py --end history`，复用已有块，从同一归档选两代，分别读回各自标记与原 crun 进程。 |

依所有者执行范围，构造解析器攻击载荷、利用漏洞或绕过访问控制的子项按要求跳过，且不计通过。
