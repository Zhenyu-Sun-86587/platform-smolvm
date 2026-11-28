# 平台与 Worker 文档

平台以 `polish/integration` 开发，已包含多 Worker 控制面、Native 和 Kernel CoW Live Fork。Runtime 在 `polish/runtime-candidate`，平台数据库 schema 为 1025，Worker 目录数据库 schema 为 32。各仓库的开发路径、已验证提交和制品口径见[源码与 Worker 预检](checkout.md)。

## 开发与使用

- [CLI 常用流程](cli-workflows.md)、[命令参考](../../cmd/sandboxctl/USAGE.md)和[公开 API](../../api/openapi.yaml)覆盖用户操作、等待与错误语义。
- [多 Worker 拓扑](multi-node-v1.md)、[Worker 证书登记](WORKER-ENROLLMENT.md)、[备份配置](BACKUPS.md)和[安装、升级与回退](../../packaging/install/README.md)覆盖部署。
- [Runtime Bundle 合同](../../packaging/deep/bundle.md)、[Kernel CoW 选择与验收](kernel-cow-closeout-2026-11-28.md)、[Fork 批次准入](batch-admission.md)和 [Guest 委派与父机模型流](guest-delegation.md)说明运行时边界。
- [日常验收与回归入口](ACCEPTANCE_AND_OPERATIONS.md)、[生产候选验收](production-handoff-2026-11-27.md)和 [macOS 构建](../../packaging/macos/README.md)用于后续验证。

下一阶段计划和行为合同位于工作区顶层 `docs/phase3/`。早期计划、冻结候选报告和实验脚本保存在本机 `platform-smolvm/archive/`。归档清单按原路径记录文件摘要。原先受 Git 跟踪的材料也可使用 `git show 69a42f4:<原路径>` 查阅。
