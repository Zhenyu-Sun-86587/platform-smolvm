# 验收与运维

## 冻结候选

记录五个仓库的源提交、平台与 Worker schema、Bundle digest 和 reader ID、Guest 模板摘要、Worker 启动内核、客户端制品摘要以及测试配置。开发机取证、独立 KVM Worker 验收与生产部署分别记录环境和覆盖范围。[源码与预检](checkout.md) · [Bundle 合同](../../packaging/deep/bundle.md)

先做只读隔离预检和安装 `--plan`，再用隔离部署验证升级、备份及回退。旧保存点恢复沿用其实际兼容 reader、Bundle 和盘代际。记录显式卷冲突策略及真实恢复结果。[安装与回退](../../packaging/install/README.md) · [备份](BACKUPS.md)

在共享开发机上，重构建使用平台 Git common dir 内 `polish-coordination/heavy.lock`。KVM 和时序、网络或压力实验取得该锁后再取得 `worker-lab.lock`，按实际准入预算启动测试 Guest。

## 常用回归

从平台仓库运行窄验证。完整生产候选使用[端到端检查清单](production-handoff-2026-11-27.md)。

```sh
go test -p 1 ./...
python3 -B -m unittest discover -s packaging/deep -p test_preflight.py -v
python3 -B tests/integration/release_paths.py -v
```

| 场景 | 现行夹具或合同 | 关键观察 |
| --- | --- | --- |
| CLI 与文件 | [CLI 端到端](../../tests/integration/polish_cli_kvm.py)、[产品与文件](../../tests/integration/polish_product.py)、[文件执行围栏](../../tests/integration/polish_file_fence.py) | 校验文件 SHA-256、上限、取消、执行代际和未知结果。受理、操作完成、Guest 可执行与服务可访问各有证据。 |
| Live Fork 与父机连接 | [Kernel CoW](../../tests/integration/kernel_cow_fork.py)、[批次重试](../../tests/integration/batch_recovery_kvm.py)、[父机流与凭据](../../tests/integration/guest_control_kvm.py) | 双子继承同一点并独立写入。原父机流继续，子机独立授权。失败子项按原捕获和原操作对账。 |
| 卷和保存点 | [多盘恢复](../../tests/integration/installed_multidisk_phase_kvm.py)、[历史 StatePoint](../../tests/integration/statepoint_history_kvm.py)、[卷生命周期](../../tests/integration/volume_product_kvm.py) | 校验盘代和捕获配置、同机回滚与从点建机、卷冲突和历史 reader。 |
| 网络和服务 | [Guest 发布切点](../../tests/integration/owned_gate_cut_kvm.py)、[Worker 网络恢复](../../tests/integration/worker_network_recovery_kvm.py)、[服务 revision](../../tests/integration/service_revision_kvm.py)、[托管 SSH](../../tests/integration/managed_ssh_access.py) | 校验执行、服务修订和绑定代际。顺序 Fork 后核对 UDP 回执，服务请求重连后按原效果对账。 |
| 生命周期与观察 | [空闲策略](../../tests/integration/lifecycle_idle_kvm.py)、[运行租约](../../tests/integration/lifecycle_lease_ui_kvm.py)、[指标代际](../../tests/integration/polish_metrics_epoch.py) | 被动 inventory、状态、metrics 和服务观察保持被动。等待模型响应的活动受保护，用户显式 Live Fork 保留独立准入。 |
| Worker 和发行 | [Worker 证书登记](WORKER-ENROLLMENT.md)、[多节点 N01–N18](multi-node-v1.md)、[整机升级重启夹具](../../tests/integration/release_host_reboot.py) | 区分同机回环和跨物理宿主。失联期间只向原归属 Node 查操作，核对心跳、保留资源和恢复结果。 |

真实 KVM 夹具需明确指定已构建的二进制、校验过的 Bundle/模板和可回收的测试资源。操作超时按原 operation ID 查询，未知写入保留原幂等键和前置条件。记录测试日志或回执的位置、候选 SHA、实际通过/未验证项和资源清理结果。[CLI 等待和错误合同](cli-workflows.md)
