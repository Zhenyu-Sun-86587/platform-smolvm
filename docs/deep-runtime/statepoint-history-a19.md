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

## 便携增量与恢复资源

冻结 `fb69434` 的库、固件和 OCI 根文件系统配合 A17 的开发 CLI `2f39d93`、Guest Agent `b09a5c0`。公开 `machine checkpoint --store` 创建两代，第二次写入 7 MiB 并复用 127 MiB。`--export-from --history 1` 发行携带两代的普通归档 `sha256:ef4c4c39047dd21db1e8f0e9fedda342cad2183b7ebf8364275896c223bcbca4`。用 `--at '~1'` 和 `--at '~0'` 分别启动两个目标，读回各自 RAM 标记、原 crun PID 480 与启动时间 97，计数分别为 14 和 27。显式请求 2 vCPU 或 768 MiB 时，CLI 分别指出原值为 1 vCPU、384 MiB，并拒绝创建。`.build/a19-portable-history-v2.log` 的 SHA-256 为 `0e36d9492d7ca05bf2f1efb41313dd9eb4136162b66f314d742438a691874f83`。[OCI 夹具](../../tests/integration/oci_agent_restart_kvm.py)

在另一台真实 KVM Guest 中，经独立只读挂载读取同一个归档 `sha256:d475053725c2f941a9e66135a2a2aa069de31b51fb46cd62109719e2979ccfaa`，Guest 实算文件 SHA-256 一致。AMD 宿主与该 Guest 的 `/proc/cpuinfo` flags 摘要分别为 `eb38fc86c301e99a8ca9db934cbfddc9d302fdaab0053a29f244888541b757fd` 和 `841a86f4bbad2596e5db45fc5cd198697cb643adfcca6611ddac52c197aabbe3`。同一开发 CLI 在 Guest 内返回 `checkpoint CPU feature contract does not match this host`，机器清单没有目标条目。宿主用同一归档的两代各启动了真实 KVM 目标。`.build/a19-portable-history-cpu-v2.log` 的 SHA-256 为 `045be931a15d21925f567c00d696b42d99eee42ad03e80d281a9d7afe2f8c483`。

```sh
sudo python3 -B tests/integration/oci_agent_restart_kvm.py \
  --bundle "$fb69434_installed" --agent "$development_agent" \
  --smolvm-bin "$development_smolvm" --end history --verify-guest-cpu-contract
```

依所有者执行范围，构造解析器攻击载荷、利用漏洞或绕过访问控制的子项按要求跳过，且不计通过。
