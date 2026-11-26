# A24 锁定平台发行的升级与回退

以平台源码 `c095c4f625c85000bee6de656c582a63ff0e8db7` 的 Git archive 构建 `0.3.0-dev.c095c4f`，使用 Linux amd64 Go 1.26.10、GCC 10.2.1 和发布脚本。发行 manifest 声明 schema 1019 和官方运行时 1.37.1，13 个成员均通过逐项 SHA-256 核验。旧平台使用实际发布的 `0.1.6`。[发布脚本](../../packaging/install/make-release.sh)

| 输入或输出 | SHA-256 |
| --- | --- |
| 新发行 `release-manifest.json` | `a1944b4f59301d8bb8560df572c8fc2912db29e7dd751f0a1054bdaeab180639` |
| 新发行 `platformd` | `370515e51659db8ee212b83b61f9736b4104b0f962e05b3d45958cf46a0f4396` |
| 新发行 `sandboxctl` | `88bf7c4c5fbbd5abfb44f85639d594a9ba36135c273150a08c1adc88560dc1eb` |
| 默认 Debian 模板 | `ad5b45167c999b4af5a845de918aff0f6c14d30544c4cadfd32c8aead95f1c64` |
| 旧发行 `release-manifest.json` | `35f222dfa87f3b65edb62638f92ede6d9b13eb0173e7fbc6bcf87edf09153968` |
| 官方 1.37.1 运行时归档 | `f8d5d1aaa198c7b9e676fa4365998e22fc9051a2199b208741be5f38fef5e404` |
| 发布输出日志 `.build/wp23/a24-final-c095c4f-build.log` | `d6f842ebbb721ff5e986ebd8adfb5519cf766d7290c2992b68b54609bdcf240d` |

## ext4 迁移中断

原 `0.1.6` 在私有 ext4 目录创建实际 owner、安装 ID、schema 2 数据库和一个旧暂停记录。迁移进程写入 4,176 字节未提交 WAL 后，控制进程仅终止该平台。旧版重开后，19 张表定义和所有行一致。相同数据库用新版成功迁到 1019，随后用旧版打开明确拒绝，数据库保持原值。

新版 CLI 原位恢复原始备份，升级后备份仍通过校验。旧版从同一原始备份创建独立恢复目录。原 owner 分别新建会话并读取各自新的 control epoch，旧暂停记录保留。`.build/wp23/a24-owner-readback-c095c4f.log` 的 SHA-256 为 `1ccbfac6f843773fb5cf18430e8c7675fac59fae60d744ad7d06d67f40c16ee4`。[迁移中断夹具](../../tests/integration/registry_migration_crash.py)

```sh
python3 -B tests/integration/registry_migration_crash.py \
  --old-platformd "$old_platformd" --old-cli "$old_cli" \
  --platformd "$new_release/bin/platformd" --cli "$new_release/bin/sandboxctl" \
  --wal-cut "$wal_cut" --schema 1019 --owner-readback
```

## 系统服务安装、升级和整机掉电

专用 Ubuntu 24.04 虚拟宿主通过原安装器安装 `0.1.6` 和官方 Serve 1.37.1。发布包由只读数据盘提供，宿主变更落入独立的 qcow2。原 owner 从公开 API 登录。升级计划核对全部新包成员，升级过程中官方 Serve PID 421 持续运行，新 `platformd` 使用 `sandbox-platform` 账户。使用新 schema 的备份请求直接回退二进制时，安装器报告会跨 registry schema 而拒绝。

同步来宾文件系统后终止本次 QEMU，从同一 ext4 再启动。两次 boot ID 为 `867f4f76-616a-4bc3-8fe5-4d405903b985` 和 `5e90cda9-85a6-42e4-954e-c78e7d593a12`。systemd 自动启动 platformd、官方 Serve 和状态 socket。数据库 inode、安装 ID 和 control epoch 保持，原 owner 成功重新登录，升级前后两份备份的核验回执逐字节相同。`.build/wp23/a24-systemd-powercycle-c095c4f.log` 的 SHA-256 为 `f1edaff7d3201cd8f8d64e8869cb2d66a4dfa0a7ccafd77131c90c97331a81c0`。[整机升级夹具](../../tests/integration/release_host_reboot.py)

```sh
sudo python3 -B tests/integration/release_host_reboot.py \
  --base-image "$ubuntu_worker_raw" --kernel "$host_kernel" --kvm-module kvm_amd \
  --old-release "$old_release" --new-release "$new_release" \
  --runtime-archive "$official_runtime_archive"
```

## 平台发行包 b66f5d9

`0.3.0-dev.b66f5d9` 使用 platform 提交 `b66f5d9c6b59d804c71442a6354325454610c3df`。发行清单的 13 个文件均逐一核对，`platformd` 与 `sandboxctl` 均来自该目录。发行清单 SHA-256 为 `9dfeb3bd367c6390d03e96537213d05b622effc9e43738313e536f2493fc981c`。下表的记录位于 `.build/release-b122bbf/`。

| 使用场景 | 结果 | 日志和 SHA-256 |
| --- | --- | --- |
| 新平台与上一版定制运行时 | 在只读安装的 `1eb5104` 上从运行中的 Debian 父机一次 Fork 出两个独立子机。三者有不同执行身份并读到原 RAM 标记，父机继续运行。 | `platform-previous-runtime-fork-v2.log` · `a5f195bc73b7a5421f906e275bec4bf4f299bbe3be97b49f3e83c4a39ff2a57f` |
| 从 0.1.6 迁移注册库和恢复备份 | 两个未提交 WAL 帧中断后，重新打开旧库并核对全部 19 张表和数据。新版迁移至 1019，原 owner 分别登录新旧版本的恢复库，成功读取暂停记录与各自的新控制代次。 | `platform-registry-previous-readback.log` · `1ccbfac6f843773fb5cf18430e8c7675fac59fae60d744ad7d06d67f40c16ee4` |
| 在独立 Ubuntu 虚拟宿主上升级并切断电源 | 官方 Serve 在升级期间保持 PID 421。虚拟宿主的 boot ID 从 `99ac49d1-34b8-4e0f-9d0c-fd4c95ef445a` 变为 `e0aa309c-87c6-49de-a295-5996bc2fc127`。systemd 自动拉起平台和运行时，原数据库 inode、安装 ID、控制代次、owner 和两份备份校验一致。 | `platform-upgrade-powercut.log` · `519f33eb56ad1ead98e0539d8dc0add60739276fc27d9da8a0c153800db0c005` |

首次跨版本 Fork 使用额外一层临时工作目录，源启动返回 `NATIVE_OPERATION_ABORTED`。沿用夹具自身的独立挂载和短工作路径后，表中的完整 Fork 通过。首次记录保存在 `platform-previous-runtime-fork.log`。
