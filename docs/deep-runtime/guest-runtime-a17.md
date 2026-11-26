# A17 Guest 工作负载与管理 Agent 生命周期

原安装为 smolvm `fb69434`，Bundle `sha256:a9b41917173a0eea2306aa41276b59fa314e1d54f9d52f45766bcf2ce7698c35`。System 用锁定平台 `c011476` 从公开 Create 启动。每次实验使用新的挂载和网络命名空间，只读绑定 Bundle，并在私有临时文件系统上运行。两个 System 测试盘由既有离线镜像复制，副本使用安装包里的 Agent 和身份 helper。Worker 的对象目录在稀疏 ext4 上，保留原有资源准入和每机 cgroup 上限。

## System 上的 Docker 与 K3s

Debian 11 Guest 的 `/proc/1/comm` 为 `systemd`，根盘为可写 `/dev/vda ext4`，运行内核为 `6.18.60`。Docker `20.10.5+dfsg1` 禁用构建缓存后从离线 BusyBox 输入构建镜像，Compose `1.25.0` 从新镜像运行实际容器，输出随机标记并返回退出码 0。普通 System Agent 由 systemd 从 PID 477 重启到 1151，原 Docker 容器保持 PID 1070、启动时间和标记。Docker 输入盘 `debian-docker-v2.raw` 的 SHA-256 为 `b2473127a6703309bffa79057638d5cd2762cdde6df0ce2bca1776ff2c77bbd4`。`.build/a17-fb69434-system-docker-v2.log` 的 SHA-256 为 `a4b2a9dd8f083b8e3a7f7e01094f41e8deb731c8dd313ae87d8f53961b858fe4`。

K3s 输入盘 `debian-k3s-v3.raw` 的 SHA-256 为 `8cd87c1fa3aa20c5d8a6ba3124bf84ef58af05e45adfbc612e36b77aeaa9d2a8`。夹具在副本里取用离线压缩镜像，创建新 datastore、节点、CoreDNS 和应用 namespace。2 vCPU、2304 MiB 下，K3s `v1.34.13+k3s1` 的节点进入 Ready，运行 `containerd://2.2.10-k3s1`，CoreDNS Pod 达到 `1/1 Running`。真实 BusyBox resolver Pod 通过 `10.73.0.10:53` 把 `kubernetes.default.svc.cluster.local` 解析为 `10.73.0.1`，把 `echo.a17.svc.cluster.local` 解析为 `10.73.0.42`，并经后一个 Service 的 HTTP 请求取得 `a17-k3s-e152c74f6a66941a`。`.build/a17-fb69434-system-k3s-v5.log` 的 SHA-256 为 `e573250c6a22597730c5b098ca7ea71c9b5e6d0ffd41613db69e203d7649db7f`。[System 夹具](../../tests/integration/system_container_workloads_kvm.py)

```sh
sudo python3 -B tests/integration/system_container_workloads_kvm.py \
  --bundle "$bundle" --platformd "$platformd" \
  --system-image "$docker_disk" --case docker
sudo python3 -B tests/integration/system_container_workloads_kvm.py \
  --bundle "$bundle" --platformd "$platformd" \
  --system-image "$k3s_disk" --case k3s
```

## OCI 管理服务重启

冻结 OCI 的 Guest PID 1 是 `krun-init`。普通 Agent 暂停时，实际 crun 工作负载继续递增。只终止该 Agent 后，Guest init 使原 VMM 退出。pidfd 回执和工作负载的原 PID、启动时间及标记均独立记录。

修复在同一 Guest 中把一次性根盘、网络、挂载准备与管理服务分开。每次管理服务新进程都重新打开控制端口，并从 crun 的原状态发现仍在运行的任务。每机私有运行状态记录原停止策略，stdout 和 stderr 由独立日志进程转发。测试使用公开 `machine create/start/exec/logs/stop`，通过原 Unix Agent 通道只对已验证的 Guest 进程注入信号。[OCI 夹具](../../tests/integration/oci_agent_restart_kvm.py)

开发 Agent `1.37.2` 的实机二进制摘要为 `sha256:891761c318146d215bedc0a2302a8b8063e5d433f02aacde702f71cce550b915`。它来自 smolvm `38cfe95`，在私有 OCI 根目录中替换 Agent。仍使用冻结 VMM、Guest 内核和普通 CLI。下列每组均核对了 VMM 的 PID/启动时间、Guest boot ID、crun 工作负载 PID/启动时间、原标记和 stdout 计数。

| 操作 | 实际效果 | `.build/` 日志及 SHA-256 |
| --- | --- | --- |
| 冻结 Agent SIGKILL | 初始管理 PID 463 的父进程是 Guest PID 1。管理暂停期间原 crun PID 487 继续，随后原 VMM 退出。 | `a17-fb69434-oci-agent-v3.log` `9cbe10a2a93bc461c0ad6d9dc4ec2fc7ae8064d7fbd6d50aaee53c924ae9f192` |
| 管理 Agent SIGTERM，随后 supervisor SIGTERM | 管理 PID 473 换为 567，crun PID 487 和启动时间 116 不变，stdout 计数越过 33。单独向 supervisor PID 463 发 TERM 后，原 VMM pidfd 确认退出。 | `a17-oci-supervisor-term-v2.log` `f5e23a1c7c4b99f6b2a2a091ae5d6313b8597bd48178870eb439dadb30684d71` |
| 管理 Agent SIGKILL，随后公开 `machine stop` | 新 Agent 接回原 crun PID 487。Stop 发送原记录中的 SIGUSR1，原进程写出带 PID 和信号的收尾标记，原 VMM 随后退出。 | `a17-oci-manual-stop-v2.log` `daf194b398104b05f57d996e3fab3200c17458612ac0115a7fe69411a9664a57` |
| 管理 Agent SIGKILL，随后原任务自然退出 | 新 Agent 接回原 crun PID 488 和递增输出。原任务自行写出退出标记，`stop_on_exit` 刷盘并经 init 所用的退出路径停止原 VMM。 | `a17-oci-stop-on-exit-v2.log` `f2bab1addb069e1837cd0aa6e0f5e90675842e9424e1658f149789918576723a` |

```sh
cc -Wall -Wextra -Werror -static -Os -s -o /tmp/oci-agent-workload \
  tests/fixtures/oci-agent-workload.c
sudo python3 -B tests/integration/oci_agent_restart_kvm.py \
  --bundle "$bundle" --probe /tmp/oci-agent-workload --expect-legacy-exit
sudo python3 -B tests/integration/oci_agent_restart_kvm.py \
  --bundle "$bundle" --probe /tmp/oci-agent-workload --agent "$development_agent" \
  --end workload
```

同一夹具可用 `--end manual` 复验显式 Stop，用 `--agent-signal TERM --end supervisor` 分别向管理服务和 supervisor 发 TERM。

smolvm `b09a5c0` 将登记文件放到 Guest 专用 tmpfs。开发 Agent 二进制摘要为 `sha256:93db40ce7acd8faccd981738237821f66b8d8c945f91ca9c46ff6747a3840cf2`。`--end branch` 启动不同宿主 PID 的 KVM 子机，实读 Guest tmpfs 内登记的 SHA-256 `054480a2d73c48b2e8e332b4b8584625e2bcb799a0186528243c3cad50bc0ba2`。子机管理 Agent 再遭 SIGKILL 后，新 Agent 取到同一登记和 crun PID 480/启动时间 91，公开 Exec 读到原进程的递增标记。`.build/a17-oci-workload-branch-v1.log` 的 SHA-256 为 `773dbba04b109e6df24a42d63ebd5f2c4d9dbad20d1b75414561b0b0d848a896`。

`--end snapshot` 使用同一个开发 Agent，从公开 CLI 生成 portable checkpoint，再建另一台宿主 PID 不同的虚机并启动。夹具为私有安装按原部署契约挂载独立的恢复暂存 tmpfs。恢复目标的 Agent 遭 SIGKILL 后，新 Agent 读到原 Guest tmpfs 登记 SHA-256 `16d1d1599066661ce584727fad6168770862dbf4ec55de4f592c64933cca3596`、原 crun PID 479/启动时间 97 及递增标记。`.build/a17-oci-workload-snapshot-v2.log` 的 SHA-256 为 `4323427933e6c84815f090af27ea0ea2eacf268c886b2ab391c172cb9e65c1ef`。

## 公开 CLI 恢复暂存

同一个冻结 CLI 在未配置专用恢复 tmpfs 的私有 loop ext4 上，生成 checkpoint 后执行 `machine create --from` 时因路径列表只有一项而 panic，退出码为 101。`.build/a17-restore-frozen-ext4-v1.log` 的 SHA-256 为 `bd610f5f4a0e6a3947aafd9e1ba73aae6fdecb84324b7470691c0cc8174782bc`。配置专用挂载时，冻结版的公开 `machine pause` 和 `machine resume` 从该 tmpfs 提取并链接 RAM，恢复后继续原 crun 进程。`.build/a17-restore-frozen-tmpfs-v1.log` 的 SHA-256 为 `9db0ee1caf3c7a3b4d00ca963ee9c7d2d8119157f0bbaefddc849e94c48503d3`。

修复 `2f39d93` 在安装只读 RAM 输入时使用已验证的暂存文件系统。缺少专用挂载时，使用机器状态目录所在的文件系统。夹具在私有命名空间为冻结 Bundle 覆盖开发版 `smolvm-bin`，二进制 SHA-256 为 `b6757ceae4a822fe9a1fa30d3f915857c57f48baf9c897b9400e3d07732e8498`，沿用上述开发 Guest Agent 和冻结 Bundle 内的 libkrun、固件及 Guest 根文件系统。三组 KVM 恢复都核对了原 crun PID、启动时间、Guest 登记及公开 Exec 取得的工作负载标记。

| 夹具参数 | 观察 | `.build/` 日志及 SHA-256 |
| --- | --- | --- |
| `--end snapshot --without-restore-tmpfs` | CLI 创建成功。目标启动前的 `.restore-input/memory.bin` 与状态目录同处 loop ext4，恢复后继续原 crun PID 479、启动时间 85。 | `a17-restore-fixed-ext4-v1.log` `f5452ade9fd47066b8fba8a7da2b8561ec325242e931178778b8e92b635cb670` |
| `--end resume` | 公开 Pause/Resume 的提取和只读链接都在配置的 tmpfs，恢复后继续原 crun PID 480、启动时间 87。 | `a17-restore-fixed-tmpfs-v1.log` `a7008316bf83dd39f0159f281b976d1a351986f06c1be69cd35c5c2efa5127a4` |
| `--end snapshot` | 配置专用 tmpfs 时，portable Create 和首次 Start 继续原 crun PID 479、启动时间 85。 | `a17-restore-fixed-snapshot-v1.log` `9a15c9acb84d8944aab43eb73d50504d6e0c083a10f7a2414b2ab98a6bcec5a3` |

```sh
sudo python3 -B tests/integration/oci_agent_restart_kvm.py \
  --bundle "$bundle" --agent "$development_agent" --smolvm-bin "$development_smolvm" \
  --end snapshot --without-restore-tmpfs
sudo python3 -B tests/integration/oci_agent_restart_kvm.py \
  --bundle "$bundle" --agent "$development_agent" --smolvm-bin "$development_smolvm" \
  --end resume
```
