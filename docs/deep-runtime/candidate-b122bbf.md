# 冻结候选 b122bbf 与平台发行 0.3.0-dev.b66f5d9

## 固定输入

| 输入 | 固定值 |
| --- | --- |
| smolvm | `b122bbfc15866f2c3b22ce8f204d93595457e7c9` |
| libkrun | `0d0255383f4378aca5be7bf19629330c08fcaf8e` |
| libkrunfw | `4d5852a59f9c8bc75780544d0fe93b5015343a32` |
| 构建校验工具 | `c095c4f625c85000bee6de656c582a63ff0e8db7` |
| 平台发行和包内验收 | `b66f5d9c6b59d804c71442a6354325454610c3df` |
| Linux 6.18.60 源归档 SHA-256 | `3c2f44ef6acc021a7ad444d9a2e6ec1ef488ff09368793d0004a6080f69d621e` |
| 运行时构建锁 SHA-256 | `339f3e471aab621eed74d324ab859025fa521fa8de63e9ba44d595b3af4fc693` |
| 工具链锁 SHA-256 | `32943b623419022f5fba0908602962791d5aa5a214a67503eec9c8f85992433f` |
| 库阶段构建记录 SHA-256 | `21f44db99b78c571776cb04e7c41c1a86acf699fccbfb4e0671589604a0125cd` |

Kbuild 和 Cargo 暂存写入私有挂载中的持久 workspace 文件系统，运行时保留发行优化并使用单 Cargo job。新锁下重新构建的 vmlinux、libkrun 与 libkrunfw 的 SHA-256 与前一候选相同。候选证据在 `.build/release-b122bbf/`。[前一冻结候选](candidate-1eb5104.md)

## 平台产品发行

从干净的 `b66f5d9` 工作区用 `packaging/install/make-release.sh` 生成 `0.3.0-dev.b66f5d9`。平台发行目录为 `/var/tmp/deepfork-smolvm-build-cache/release-b122bbf/platform-release`。原官方运行时清单对应 1.37.1，registry schema 为 1019。深度运行时验收使用同一平台发行中的 `bin/platformd` 和 `bin/sandboxctl`。

| 平台发行物料 | SHA-256 |
| --- | --- |
| `release-manifest.json` | `9dfeb3bd367c6390d03e96537213d05b622effc9e43738313e536f2493fc981c` |
| `bin/platformd` | `da28859b6b3ad8b29ddef4c28eee834b93575fe01e6d85111f6fb291ac0df77b` |
| `bin/sandboxctl` | `6b2cf6f094b2cb0c7217242c09ed7dfae1fafa00bb54c2d7cc3e645bfb4319ec` |
| 官方 `runtime-manifest.json` | `5dc79c17bfae5e00b62471fdb1407df5a89908c37ba594b9ea1791e9386bf6be` |
| 已验 Debian 管理模板归档 | `ad5b45167c999b4af5a845de918aff0f6c14d30544c4cadfd32c8aead95f1c64` |

清单的 13 个成员全部按字节校验，CLI 输出正式版本。两份 Go 构建信息都记录预期提交及 `vcs.modified=false`。内嵌前端的 Git 树 `b06acba4640a07656c2eaf1a5dbff2b824805b0e` 与前一候选相同。逐项回执见 `platform-release-receipt.json`，源码比较见 `platform-frontend-inputs.log`。[平台发行脚本](../../packaging/install/make-release.sh)
