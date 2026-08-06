# Phase 2 实现与验收

## 实现范围

Phase 2 把 Phase 1 的 USB 控制底座扩展为可运行的 IPv4 用户态 VPN。

### HarmonyOS NEXT

- `VpnExtensionAbility` 独立拥有 VPN 生命周期，不把 live fd 交给 UI Ability。
- UI 与 VPN Extension 使用仅限本 bundle 的 CommonEvent 同步状态和停止请求，避免依赖不可靠的跨进程静态变量。
- ArkTS Control socket 完成 `mode=vpn` 握手、`VPN_STATUS`、`STOP_REQUEST/ACK`。
- Native C++ Data socket 使用 Control handshake 产生的随机 token 完成关联。
- Control/Data 两个 socket 均在创建 VPN 前调用 `VpnConnection.protect(fd)`。
- VPN 配置使用 `198.18.0.2/30`、默认 IPv4 路由 `0.0.0.0/0`、虚拟 DNS `198.18.0.1` 和 MTU 1400；IPv6 当前关闭。
- Native `PacketPump` 使用两个线程完成 TUN → Data 和 Data → TUN，全程按完整 HNB/1 `IP_PACKET` 帧传输。
- 任一数据面错误、Mac STOP 或 Extension 销毁都会先停 PacketPump，再销毁 VPN。
- App 内置 UDP DNS 与 TCP DNS 自检，目标固定为虚拟 DNS，不依赖设备原有网络路径。

### macOS

- daemon 接受独立 Control/Data 连接，验证 token、角色、连接顺序和唯一性。
- `relay.Engine` 用固定版本 gVisor Netstack 终止设备侧 IPv4 TCP/UDP 流，并在 Mac 上创建普通 `tcp4` / `udp4` socket。
- 虚拟 DNS 同时支持 UDP 与 TCP。
- `SystemDNS` 从 `/usr/sbin/scutil --dns` 获取 resolver，按查询名的最长后缀匹配 split DNS，并遵循 resolver order 与自定义 port。
- DNS 配置每 30 秒刷新；刷新失败时只使用上一次成功快照，不回退公共 DNS。
- 日志和状态只记录生命周期及计数，不记录 packet、域名、目标地址或 session token。

## Gate V 结果

在进入完整默认路由前，项目在真实 HarmonyOS NEXT 手机上通过了最小 Gate V：

- 系统接受第三方 VPN 用户授权；
- Control socket fd `protect()` 成功；
- 受限 `/32` VPN 创建成功；
- Native 层从真实 TUN 读到匹配的测试 UDP packet；
- `destroy()` 成功，Extension 正常退出。

这证明公开 `VpnExtensionAbility` 能力在当前测试设备上可用于第三方用户态 VPN。它不代表所有机型、企业策略或应用市场环境都会允许同样操作。

## 自动化覆盖

运行：

```bash
./scripts/check.sh
```

主要覆盖：

- HNB/1 所有头字段、长度边界、分片/粘包和 sequence。
- VPN Control/Data 握手、token 错配、双向 packet relay、断连与 stop。
- gVisor 内存端到端 TCP echo、UDP echo、UDP DNS 和 TCP DNS。
- macOS `scutil --dns` parser、默认 resolver、最长后缀 split resolver 和无公共回退。
- ArkTS DNS query、错误响应、TCP 长度前缀和分片响应。
- Go race detector、`go vet`、macOS arm64 build、ArkTS type check、Native CMake/Ninja 与签名 HAP 打包。

## 真机验收步骤

1. 安装匹配设备签名的 Debug HAP。
2. 运行 `./bin/harmony-netbridge start`，确认 `PORT_READY`。
3. 打开 App 并点击“启动 VPN”。首次运行确认系统 VPN 授权。
4. 运行 `./bin/harmony-netbridge status`，确认：

   ```text
   Transport: DATA_CONNECTED
   VPN:       ACTIVE
   ```

5. 在 App 点击“运行 TCP / UDP / DNS 自检”，确认 `PASS`。UDP 与 TCP 查询必须分别收到回答。
6. 运行 `./bin/harmony-netbridge stop`，确认设备日志依次出现 STOP、VPN destroy 成功和 Extension destroyed。
7. 验证停止后 Mac owned hdc mapping 被删除，设备无残留默认 VPN 路由。

## 当前实测证据（2026-08-06）

| 验证项 | 级别 | 结果 |
| --- | --- | --- |
| Gate V 授权、protect、create、TUN read、destroy | 真实 HarmonyOS NEXT 手机 | 通过 |
| Phase 2 Control/Data handshake 与默认 IPv4 VPN | 真实 HarmonyOS NEXT 手机 + USB | 通过 |
| Mac `status` 达到 `DATA_CONNECTED / ACTIVE` | 真实 HarmonyOS NEXT 手机 + USB | 通过 |
| Mac STOP → PacketPump stop → VPN destroy → Extension exit | 真实 HarmonyOS NEXT 手机 + USB | 通过 |
| TCP/UDP/DNS App 自检 | 真实 HarmonyOS NEXT 手机 + USB | 通过：UDP DNS 1 条回答；TCP DNS 1 条回答 |
| TCP/UDP/DNS relay | gVisor 内存端到端测试 | 通过 |
| 企业内网与 AnyConnect split DNS | 特定公司环境 | 尚未验证 |
| 长时间锁屏、拔线、吞吐和自动恢复 | Phase 3 稳定性测试 | 尚未验证 |

最终回归还确认了两种停止入口：App 内“停止 VPN”通过 bundle 内受限事件触发有序清理；Mac CLI 会等待有界的 `STOP_ACK` 后再关闭连接。两条路径均观察到 `PHASE2_VPN_DESTROY_OK`，没有残留活动 VPN。

## 已知限制

- 只支持 IPv4 TCP/UDP/DNS；IPv6、ICMP 和原始协议不在 Phase 2 MVP 内。
- Data channel 断开会安全失败并销毁 VPN，但不会自动重连。
- 一次 daemon 只关联一个 Control/Data 会话和一个设备。
- MTU 固定为 1400；不同企业 VPN 的最佳 MTU 尚未自动探测。
- DNS 只使用 Mac 当前可见的 IPv4 resolver。只有 IPv6 resolver 的网络当前不可用。
- 当前自检验证 DNS-over-UDP 与 DNS-over-TCP，因此同时覆盖 UDP、TCP 和 DNS 数据面；它不是吞吐基准。

## Phase 3 计划

1. Control/Data 心跳和指数退避自动重连。
2. TUN、HNB、TCP/UDP/DNS 计数暴露到安全的 CLI status，不记录流量内容。
3. 30/60 分钟锁屏、拔插 USB、daemon 崩溃和 App 进程重启矩阵。
4. MTU 配置与路径 MTU 验证。
5. `devices` 命令和显式多设备会话隔离。
6. IPv6 relay 可行性及企业 split DNS 真机验证。
