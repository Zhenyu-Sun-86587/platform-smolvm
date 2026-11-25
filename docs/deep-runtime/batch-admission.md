# Fork 批次准入

`POST /v1/machines/{machine}/fork-batches` 默认采用 `all_or_reject`。Worker 在一次预算事务中为整个批次预留资源。任何一个孩子超出预算时，事务回滚，源继续运行。

显式传入 `admission: "best_effort"` 时，Worker 按请求中的顺序预留可用容量。HTTP 接受响应保留所有请求的孩子 ID 和顺序。Worker 返回准入结果后，操作的 `result.admitted_child_ids` 列出获准的孩子。超预算的孩子保留原操作中的 ID，并返回 `RESOURCE_LIMIT`。整批容量不足时，操作返回 `RESOURCE_LIMIT`，源保持原状态。

获准的孩子共享一次源捕获。重放同一幂等请求会返回既有孩子和准入记录。批次重试适用于已获准且启动失败的孩子，并使用原捕获。一次预算拒绝后的新尝试使用新的批次请求。

```sh
sandboxctl machine fork-batch "$source" first-child second-child --admission best_effort
```

## Worker 恢复实测

测试使用两块写盘和实际 Linux KVM。它先验证公有 API 的两种准入策略，再结束祖先、重启 Worker。存活子进程重新提供原封存 RAM 和写锁所用的文件描述。验证保持子执行 PID 和写盘绑定，随后从这个子执行经同一公有 API 派生一代并读取连续的 Guest 计数。

```sh
sudo python3 tests/integration/batch_recovery_kvm.py --smolvm "$smolvm_checkout" \
  --platformd "$platformd" --bundle "$runtime_bundle" --guest "$bench_guest_clean"
```

2026-11-26 运行记录在 `.build/wp23/tracked-provider-kvm.log`。此夹具使用无 TAP 的 OCI Guest。
