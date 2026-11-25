# Fork 批次准入

`POST /v1/machines/{machine}/fork-batches` 默认采用 `all_or_reject`。Worker 在一次预算事务中为整个批次预留资源。任何一个孩子超出预算时，事务回滚，源继续运行。

显式传入 `admission: "best_effort"` 时，Worker 按请求中的顺序预留可用容量。HTTP 接受响应保留所有请求的孩子 ID 和顺序。Worker 返回准入结果后，操作的 `result.admitted_child_ids` 列出获准的孩子。超预算的孩子保留原操作中的 ID，并返回 `RESOURCE_LIMIT`。整批容量不足时，操作返回 `RESOURCE_LIMIT`，源保持原状态。

获准的孩子共享一次源捕获。重放同一幂等请求会返回既有孩子和准入记录。批次重试适用于已获准且启动失败的孩子，并使用原捕获。一次预算拒绝后的新尝试使用新的批次请求。

```sh
sandboxctl machine fork-batch "$source" first-child second-child --admission best_effort
```
