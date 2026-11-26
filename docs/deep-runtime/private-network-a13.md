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

跨域组只向本域已配置的自有地址发送。冒用 attachment、规避访问控制和 IPv6 旁路的子项按项目所有者要求跳过，且不计通过。
