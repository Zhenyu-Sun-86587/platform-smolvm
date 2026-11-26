# A22 服务修订与已授权流

在私有挂载、网络命名空间和专用 systemd slice 内启动 Linux x86_64 KVM System。使用 `f2df146` 正向测量 Bundle `sha256:1d79bf8139035f924f8132fe58837686d6da9a0c72bc2c3ee2f32829882bedff`，System 镜像 SHA-256 为 `4b5ba5301eac04a398f50fb5be2f969c6e793845d249303783dc2f69a3a3c23f`。平台、HTTPS 网关和 mTLS 监听器都在同一试验网络命名空间。客户端正常校验该试验 CA，owner 发放作用域只覆盖自有机器或服务的凭据。

## HTTP 与 TCP mTLS

三组均使用包含 `ea136df` 和 `17bc2ad` 的开发平台，二进制 SHA-256 为 `7a1c13411b276606a17c1564c4279f4117fd5f403f45f30d8d6582458bc36725`。HTTP 使用连续 SSE，TCP mTLS 使用连续编号文本帧。两个 Guest 后端各自确认真实业务到达并记录次数。

1. 合法连接在第一个后端收到连续帧。公开 PATCH 把服务修订从 1 改为 2，Guest 端口从 18111 改为 18112。旧流关闭，两条随后建立的授权连接均到新端口。
2. Owner 撤销其中一条已经建立的流所用的令牌或证书。该流关闭。夹具随后在 Guest 写入新标记，另一条持有效凭据的原连接收到携带该标记的编号帧。
3. 前后执行引用保持一致。Guest 的后端收据确认三个业务 ID 各到达对应后端一次。

表中的时间从发起公开操作到客户端观察到流关闭，单位为毫秒。[KVM 夹具](../../tests/integration/service_revision_kvm.py)

| 入口 | 旧修订流关闭 | 已授权流凭据撤销后关闭 | `.build/` 日志及 SHA-256 |
| --- | ---: | ---: | --- |
| HTTPS SSE | 784 | 764 | `a22-http-revision-v5.log` `5b99383f812ab273f156d3351825945d9c7b9a94f7f4a382c44617052b39e6bc` |
| TCP mTLS | 848 | 819 | `a22-tcp-mtls-revision-v5.log` `6b7444fc4654abf8f754574e574ace519515bbaf00c6f7cb4cccfb60a427549c` |

## SSH 修订与精确唤醒

普通 OpenSSH 通过 `sandboxctl ssh-proxy` 建立到自有 Guest 的连接，并按控制面公布的 host key 严格校验。服务名称变更使修订从 1 增到 2，已经收到两个连续帧的会话在 778 毫秒后被观察到关闭。新的合法会话在原执行写入 RAM 标记。

显式 Hibernate 保存 `point-5dcd014072755d5f59cbce4d5cf5936b`，pidfd 确认旧 VMM PID 981139 退出。持有效凭据的 OpenSSH 正常触发一次 `vm_restore`，Worker 回执引用这个保存点并到达 `released`。新会话校验同一 host key 并读回 `/run` 标记，原 instance ID 保留，incarnation 更新，epoch 从 1 到 2。`sandboxctl` 的 SHA-256 为 `462a66b2b53d14553c607b378d6ef6f328300621e021cef4eead780a20f11ca7`。`.build/a22-ssh-revision-v3.log` 的 SHA-256 为 `c96de0c1ce3facb523d3c3dba0d771aea64cea08edc6762440826c16bb3ccaca`。

```sh
for case in http tcp-mtls ssh; do
  sudo python3 -B tests/integration/service_revision_kvm.py \
    --bundle "$measured_bundle" --platformd "$platformd" \
    --system-image "$system_disk" --sandboxctl "$sandboxctl" --case "$case"
done
```

撤销场景只对已合法建立的连接观察关闭。使用他人或失效凭据连接的子项按项目所有者要求跳过，且不计通过。
