# Phase 3 实现与验收

## 范围

Phase 3 在已通过真机验证的 Phase 2 IPv4 VPN 数据面上补齐单设备稳定性能力。本阶段明确不实现 `devices` 命令、多设备并发或同时维护多个 hdc target。

已实现：

- CLI `start --mtu <576...1500>`，默认 1400；Mac 通过 `HELLO_ACK.mtu` 下发，Harmony VPN 与 gVisor relay 使用同一值。
- Control/Data 两条连接各自维护 HNB sequence、5 秒心跳和 15 秒确认超时。
- ArkTS Control 20 秒入站 watchdog，以及 Native Data 20 秒阻塞读取上限。
- 非主动断线后有序停止 PacketPump、销毁旧 VPN，并以 1/2/4/8/10 秒上限退避重建完整会话。
- daemon 非正常退出后，从受保护的状态文件读取并只移除本实例上次记录的精确 hdc 映射，再创建新映射；不会批量清理其他端口。
- App 停止、CLI `STOP_REQUEST` 与系统销毁是终止事件，不会触发重连。
- DNS resolver 30 秒常规刷新；旧 resolver 交换失败时立即重新读取 `scutil --dns`；UDP 截断响应自动改用 TCP。
- `status` 输出 MTU、运行时长、双通道 RTT、重连次数、双向包/字节数和 TCP/UDP/DNS 聚合计数。

统计与日志不包含 packet payload、源/目标地址、端口、DNS 查询名、完整设备 ID 或 session token。

## 状态机

```text
STARTING
   │ Control + Data + protect + VPN create + PacketPump
   ▼
 ACTIVE ───────────────────────────┐
   │                              │ user / CLI / system stop
   │ transport or pump failure    ▼
   ▼                            STOPPED
RECONNECTING
   │ destroy old VPN + bounded backoff
   └──────────────► STARTING
```

每次重连创建新的 Control session token，旧 token 不能绑定新的 Data socket。daemon 仍只接受一个活动 Control 和一个匹配的 Data 连接。

## 自动化验收

运行：

```bash
./scripts/check.sh
```

覆盖内容：

- Go `-race` 全量测试、`go vet` 与 macOS arm64 构建。
- MTU CLI 边界、HNB `HELLO_ACK.mtu`、Control/Data 心跳、连续 sequence、RTT 状态和重连计数。
- gVisor TCP/UDP/DNS relay 聚合统计。
- macOS resolver 故障刷新及 UDP DNS 截断后的 TCP retry。
- ArkTS MTU 与退避策略测试、Native C++ 双向数据/心跳编译、签名 HAP 打包。

## 真机验收步骤

1. 安装签名 HAP，运行 `./bin/harmony-netbridge --mtu 1280 start`。
2. App 启动 VPN，确认 CLI 为 `DATA_CONNECTED / ACTIVE / MTU 1280`。
3. 等待至少一个周期，确认 Control/Data heartbeat 均有 RTT。
4. 在 App 运行 UDP DNS 与 TCP DNS 自检，确认 `PASS`，并确认 CLI 聚合计数增长。
5. 精确终止当前 daemon 进程以模拟崩溃，不发送 `STOP_REQUEST`；App 应进入 `RECONNECTING` 并销毁旧 VPN。
6. 重新运行同一 MTU 的 daemon；无需再次点击，App 应自动恢复 `DATA_CONNECTED / ACTIVE`，`Reconnects` 增加。
7. 再次运行网络自检，最后执行 CLI `stop`，确认 App 为 `STOPPED`、VPN 销毁且 hdc 映射移除。

## 本轮真机结果（2026-08-06）

本轮在一台已授权 USB 调试的 HarmonyOS NEXT 真机与 Apple Silicon Mac 上完成：

- Phase 1 `hello / world` 成功，随后以 `--mtu 1280` 启动完整 VPN；CLI 与 App 均报告 `DATA_CONNECTED / ACTIVE / MTU 1280`。
- Control/Data 心跳均持续返回；最终安装版实测 RTT 为 5 ms / 2 ms。
- 最终安装版网络自检为 `PASS`：UDP DNS 1 条回答、TCP DNS 1 条回答；同一时刻 CLI 统计为 TCP 1、UDP 1、DNS 2，双向 packet/byte 计数同步增长。
- 两次精确强制终止 daemon 后，App 进入 `RECONNECTING` 并销毁旧 VPN。每次重启均未人工删除映射；新 daemon 只移除状态文件记录的旧映射并成功恢复 `ACTIVE`，`Reconnects` 依次为 1、2。
- 恢复后的网络自检再次为 `PASS`，证明恢复的不只是控制状态，TUN → USB → Mac Relay 数据面也已重建。
- 最终执行 CLI `stop` 后，App 显示 `STOPPED / VPN 已安全销毁`，daemon 进程不存在，设备端口 27183 对应的项目 hdc 映射计数为 0。
- 前台状态卡已在 VPN 活动时显示 `VPN_ACTIVE / Mac USB 通道已接管`，不再把独立 Phase 1 会话的断开状态误报成 VPN 断开。

上述为本轮短时真机功能与故障恢复证据；不等同于跨小时压力、Mac 休眠唤醒或吞吐基准结果。

## 当前限制与下一阶段

- 仍只支持 IPv4 TCP/UDP/DNS；不转发 IPv6、ICMP 或任意原始协议。
- 自动重连不能让已物理拔出的 USB、关机设备或被系统策略撤销的 VPN 权限凭空恢复；外部条件恢复后会继续尝试。
- 跨小时、Mac 休眠唤醒和高吞吐基准仍需单独执行。
- Phase 4 的受管 mitmweb 抓包模式已经实现，见 [`phase-4.md`](phase-4.md)；多设备并发不在当前计划内。
