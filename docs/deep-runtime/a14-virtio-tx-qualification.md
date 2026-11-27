# A14 virtio TX 复验

2026-11-27 在本机隔离的 L1 Worker 上使用 Ubuntu `6.8.0-151-generic`、4 vCPU、8 GiB RAM 和嵌套 KVM。每个用例从相同 Worker 镜像启动，在新 mount/network namespace 内从只读 payload 装入同一候选。源和子机使用 `fixed_internal`，独立接收器使用 `native`。每台 System Guest 为 1 vCPU、512 MiB RAM。

| 输入 | 标识 |
| --- | --- |
| Runtime Bundle | `sha256:9b98e111bdf70daa23bf54fffa44752a55767ea0137f0e4c0b82eaec4232d103` |
| Runtime | `afca00975d92d49a9bea6571ad7c6dc6e4c8b691` |
| libkrun | `2a330f18b942b1eb22466a8b27c3e17a39582a5a`，`lib/libkrun.so.2` SHA-256 `6f251c4c49b4831adfef90c96062644c1d49aa877597fc5c43e4246685e99925` |
| platformd | `4e1ba470ed66df319f9fc79f6f0d391a3d0914bb`，payload 中的 SHA-256 `c1bdbc915c97d0f1c4e1c28b67fa8ee4a4ed3bcf412f5b50f2f3f88ddefa86ee` |
| Debian System 盘 | SHA-256 `866846e8c9e3f2fd491123450d22908aac4624c089e4e71d8370c249360ff0cc` |
| 验收脚本 | `a61c24d8052463716febcf7ae7fc87dba4e3d380`，SHA-256 `af728d9c7e103d4eff09e5baf098c24966c2b81e6a67f0505a190086e7f3c101` |
| 只读 payload | SHA-256 `6a2aabe667a51e52d64a7f831aa1c173b4c8d7b641fe0a81e0ca17bc9f386c11` |

libkrun 处理 TAP 发送失败时，将对应描述符归还给 Guest，并保留已完成描述符的中断通知。遇到 `EAGAIN` 则保留待发送帧。该提交的三项 net worker 单测通过，构建日志位于 `.build/worker-kernel-closeout/a14-stock/build-vmm.log`。以前候选的接收器曾看到子机首批心跳，随后错过正常业务包。[历史记录](A14_OWNED_GATE.md)

## 顺序 Fork

父机经公开 API 连续完成两次单子 Fork，对应 `ram-776fa59a7beedc74bae1bfb9ce40333e` 和 `ram-3dfdc32719658c4f20d944dcb550303f`。两次均返回 `effect=applied`，源锁释放，父机保持原 incarnation 和 epoch。两台子机均为 `ready`，公开回执标记资源独立性已验证。三台发送者的来源 IP、hostname 和 machine ID 各异。

独立接收器 `10.44.61.2` 收到 17 个互异的业务 token。每次 Fork 前各有一笔父机业务，第一次 Fork 后有六笔，第二次后有九笔。每个 token 对应恰好一条 UDP 收据，与发送方的本机回环响应逐字段一致。第一次 Fork 后，父机和子一的三个业务窗口分别覆盖 13.465 s 和 12.429 s。第二次 Fork 后的三组收据如下。

| 发送者 | 实际来源 | 三次业务计数 | 首末收据观测间隔 |
| --- | --- | --- | --- |
| 父机 | `10.44.61.3` | 86 → 104 → 121 | 13.648 s |
| 子一 | `10.44.61.4` | 105 → 123 → 139 | 13.639 s |
| 子二 | `10.44.61.5` | 76 → 92 → 108 | 12.780 s |

本次 Worker slice 峰值为 447,451,136 字节。串口日志 `.build/a14-final/sequential.serial` 的 SHA-256 为 `01f3377d95c4b62aca1f519fee2e225b9fba2ce748980c742b8fcee2d745c56d`。

## 激活切点

夹具核对退出前后的目标执行代际和激活 revision。每个通过的用例都验证了公开 Fork 终态 `failed`、源锁释放、父机保持原身份继续处理业务，以及失败目标的 `ABORTED/failed_held` 状态。失败进程已退出，writer grant 已释放且 reservation 数为 0，TAP 已退休且无发布记录，公开机器列表未出现失败目标。独立接收器从失败目标地址观察到 0 条数据，并收到随后另一次 Fork 的成功子机三条不同业务收据。

| 切点 | 退出前后 → 终态 revision | 持久决议 | 公开 effect | 成功子机三笔对账用时 | Worker slice 峰值字节 |
| --- | --- | --- | --- | --- | ---: |
| `authority_prepared` | 1/1 → 2 | `none` | `none` | 14.576 s | 418,258,944 |
| `offered` | 2/2 → 3 | `none` | `none` | 13.581 s | 421,072,896 |
| `applying` | 3/3 → 4 | `none` | `none` | 12.771 s | 644,988,928 |
| `identity_ready` | 4/4 → 5 | `none` | `none` | 13.558 s | 422,944,768 |
| `committing` | 5/5 → 7 | `commit` | `unknown` | 12.948 s | 429,944,832 |
| `policy_armed` | 6/6 → 7 | `commit` | `unknown` | 14.842 s | 420,925,440 |

公开 effect 由持久化激活决议确定。接收器的 0 条计数反映对应实验的私网业务观测。

| 串口日志（`.build/a14-final/`） | SHA-256 |
| --- | --- |
| `authority_prepared.serial` | `232a9c705c8214eae716b021fcc1199b295acc1db140cf6dfca100fb0b6c4ea7` |
| `offered.serial` | `388599088305f53abc10176724d27f4e743ad185339b8c02f48b7c6c96c79922` |
| `applying.serial` | `625c884461090fc755842585464f866fb07145dbe89e75f2a2fc658a2cf57cc2` |
| `identity_ready.serial` | `af4c797bf85d50e0428d4ed7f6495edd10a08a62c1df10c7df2966ed1ba70fad` |
| `committing.serial` | `1b8262af5fe7fbd82dd02870651a00f9a8f7f256c5cdabbc8f242b28cd28a7ed` |
| `policy_armed.serial` | `5115272fad6f0c7b12a4d03dca12fb92390cb714477bc5c9525beff9a806131c` |

七项最终执行均记录 `COW_SUITE_EXIT=0` 和 `runner_exit=0`。首次启动 `identity_ready` 时 runner 中断，已归档当次空串口。校验只读 payload 摘要后，从同一镜像继续执行最后三个切点，复用前四项已完成的记录。

| 汇总证据（`.build/a14-final/`） | SHA-256 |
| --- | --- |
| `qualification.json` | `56e912cf1aef52f427b746d4ab259c21f56102015f85ba86f6655c0a2c0f608c` |
| `run.log` | `cfbc0824e5fa02432aaec4c9b29500e1e00c9da3b3d873ba9a8075652ab4342e` |

## 复现

在自有隔离 Worker 内以 root 从已安装目录复验。

```sh
for phase in sequential authority_prepared offered applying identity_ready committing policy_armed
do
  python3 -B tests/integration/owned_gate_cut_kvm.py \
    --use-installation --bundle "$bundle" --platformd "$platformd" \
    --system-image "$system_disk" --phase "$phase" --cut-timeout 120
done
```

顺序测试由同一父机完成两次公开 Fork。三个时间窗相隔至少 6 秒。夹具为每笔回环业务生成唯一 token，逐条与独立接收器记录的 UDP 来源、计数、hostname 和 machine ID 匹配。切点测试以只读目录观察同一目标的 boot、执行代际、PID 起始时间和 revision，再经该目标的 pidfd 结束自有进程。旧 ACK 重放和伪造身份按既定范围记为跳过。
