# A13 自有实例的私网互通

三组均在私有挂载和网络命名空间内以真实 Linux x86_64 KVM 启动两个自有 Debian System。使用 `f2df146` 正向测量 Bundle `sha256:1d79bf8139035f924f8132fe58837686d6da9a0c72bc2c3ee2f32829882bedff`、平台二进制 `sha256:7a1c13411b276606a17c1564c4279f4117fd5f403f45f30d8d6582458bc36725`，及 System 镜像 `sha256:4b5ba5301eac04a398f50fb5be2f969c6e793845d249303783dc2f69a3a3c23f`。每个方向由目标 Guest 的 TCP 或 UDP 应用读取真实源地址并返回自己的标记。Worker 查询确认实际 attachment 与路由域，Guest 读取本身的 IPv4 与接口 MTU。[KVM 夹具](../../tests/integration/authorized_private_network_kvm.py)

| 场景 | 实际看到的地址和业务 | `.build/` 日志及 SHA-256 |
| --- | --- | --- |
| 同一个域内两个 FixedInternal | 两个 Guest 的内部地址均为 `10.0.2.15`，外部 VIP 分别为 `10.44.62.2` 与 `10.44.62.3`。双方通过对方公布的 DNS 名建立正常 TCP，各自读到对端的 VIP 和业务标记。两台都经各自 DNS 名和 VIP 完成 hairpin，目标应用看到本地网关 `10.0.2.2`。 | `a13-same-domain-v1.log` `e675a5e3972612cabee8a0c0dc4eeca3049ffc518d9bb111d9671007a24e51a1` |
| 同一个域内 FixedInternal 与 Native | FixedInternal 仍为内部 `10.0.2.15`、外部 `10.44.62.2`，Native 的 Guest 和私网地址均为 `10.44.62.3`。真实 TCP 双向分别看到 `10.44.62.2` 和 `10.44.62.3`，FixedInternal 自身 DNS 与 VIP hairpin 也成功。 | `a13-mixed-v1.log` `e83a0d46355f5a43d76c8bbaf6ecf1084dafcb3fae5ec289fd462c284170ef5d` |
| 两个独立域和相同 CIDR | `a13-alpha` 与 `a13-beta` 都使用 `10.44.62.0/24`。两个独立 attachment 都分到 `10.44.62.2`，各自的 DNS 名解析到本域地址。各 Guest 对该地址和自身 DNS 的正常 hairpin 都返回自己的后端标记。 | `a13-overlap-v1.log` `0bdb26c25e3fa23b3465b539dd8b64b3867129cd4895c7973398ea9000443735` |

同域的两组还进行了正常 UDP。双方的实际接口 MTU 为 1500，设置 IPv4 DF 后双向完整交付 1472 字节应用载荷，1473 字节由发送 socket 返回 `EMSGSIZE`。目标应用在收到最大载荷时报告的源地址与 TCP 相同。

```sh
for case in same-domain mixed overlap; do
  sudo python3 -B tests/integration/authorized_private_network_kvm.py \
    --bundle "$measured_bundle" --platformd "$platformd" \
    --system-image "$system_disk" --case "$case"
done
```

重叠组只向本域已配置的自有地址发送。冒用 attachment、规避访问控制和 IPv6 旁路的子项按项目所有者要求跳过，且不计通过。

## 显式对等路由

同一 Worker 已配置两个自有私网时，主机操作员可通过 `smolvm-resourced serve --private-network-peering-config "$policy"` 启用 IPv4 双向直连。文件为 root 所有，且只允许 root 写。`left` 和 `right` 填写 Worker 登记的 `network_id`。

```json
{
  "version": 1,
  "allowed_pairs": [
    { "left": "alpha", "right": "beta" }
  ]
}
```

Worker 等待两个已登记私网的路由器可用，核对两域 CIDR 和 FixedInternal Guest 的内部路由无歧义，再在两域之间安装独立转发链和对端路由。Native Guest 保留私网源地址，FixedInternal Guest 由原 attachment 映射为自身 VIP。Guest 可直接连接对端的私网地址。域名解析仍使用各私网的 DNS，跨域自动唤醒沿用已授权的 Gateway 入口。每对私网分别授权。转发链匹配双方可分配的地址范围，实际端点仍须通过各自 attachment 的原有规则和主机路由。

原子替换策略文件可调整授权。使用空 `allowed_pairs` 撤销全部对等路由。Worker 在下一次协调时移除对应转发链和路由。Worker 退出时，Linux 回收对等网络命名空间以及对应的两侧链路。Worker 重启恢复原 attachment 后，按当前文件重新应用策略。

```sh
printf '%s\n' '{"protocol":"WorkerResource/1","request_id":"peering","method":"QueryPrivateNetworkPeering","payload":{}}' |
  smolvm-resourced call --socket "$worker_socket"
```

`requested` 记录已读策略，`active` 记录已装配的双向路由，`error` 记录策略或装配问题。Worker catalog 恢复期间返回 `E_RECOVERING`。源或目标遇到网段重叠时，先消除配置歧义，Worker 才能建立该路由。

五个独立 Linux 网络命名空间中的内核窄测同时配置 A↔B 和 A↔C。A 的两个路由均以 `169.254.1.1` 为下一跳，各自拥有独立设备和 `PERMANENT` 邻居记录，两对均通过双向 ICMP。关闭 A↔B 的转发命名空间后，A 和 B 上对应路由消失，A↔C 的实际双向通信继续。`.build/a13-peering-netns-v2.log` SHA-256 为 `d081081ed311265a0bb48e2a7d439dc74fd92357319647a95719e157dc97b60d`。

## 两个已授权私网的 KVM 实测

开发 Bundle `sha256:0b9e399f082e7b021a4cd078711b5bcd46843bdbb8c08b1f6f4bae923e4232b5` 使用上述 Serve、平台和 System 镜像，以及从 `1eb5104` 单独构建的 Worker `sha256:024d00af15399915a5ae399a495fae9448c99af73f846f625a59451ee9f52e0a`。Rust 窄测通过双边登记、撤销配置和两个重叠情形。`.build/a13-peering-worker-build-v1.log` SHA-256 为 `09db1ff022a9dab7d01e1bebe740f463b94198d2df74fa33e811698d4a2eed7c`。

两个真实 Debian System 分别位于 `10.44.62.0/24` 和 `10.44.63.0/24`。空策略时两边均无对端路由。写入授权后，FixedInternal 目标应用读到 Native 源 `10.44.63.2`，Native 目标应用读到 FixedInternal VIP `10.44.62.2`，两次正常 TCP 请求各收到对端业务标记。

停止测试自己的 Worker 后，两侧对端路由消失。复起同一 Worker 二进制进入新的 broker session，重新安装两个对端路由，两台 Guest 保留各自原 incarnation 和 epoch 1。新一轮获准 TCP 双向请求再次成功。操作员撤销授权后，查询回执与两边的 Linux 路由表均显示已撤除，两台原执行仍可通过各自的 Guest Agent 读取 PID 1 为 `systemd`。`.build/a13-authorized-peering-kvm-v1.log` SHA-256 为 `f3cdae8f09f50563d632a45ac94fdec0c34d8b7d5cfaa3a417d86d4240925112`。

```sh
sudo python3 -B tests/integration/authorized_private_network_kvm.py \
  --bundle "$peering_bundle" --platformd "$platformd" \
  --system-image "$system_disk" --case peering
```
