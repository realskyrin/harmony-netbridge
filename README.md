# HarmonyNetBridge

HarmonyNetBridge 是一个面向 HarmonyOS NEXT 开发者的开源 USB 网络桥。它不依赖 Android API，让鸿蒙设备通过 USB 与 `hdc` 复用 Mac 当前的网络路由，包括企业 Wi-Fi、Cisco AnyConnect 等开发环境。

当前仓库已实现 **Phase 3 单设备稳定版**：在 Phase 2 IPv4 VPN 数据面之上加入可配置 MTU、Control/Data 双通道心跳、断线自动重连、DNS resolver 失效刷新和安全流量统计。HarmonyOS `VpnExtensionAbility` 接管默认 IPv4 路由，Mac 端通过 gVisor Netstack 复用当前网络与企业 split DNS。

## 当前能力

- `harmony-netbridge start/status/stop`：管理单设备 daemon、`hdc rport`、实时状态和安全停止。
- 独立的 HNB/1 Control/Data TCP 连接，以随机 session token 关联，避免控制帧与高频 packet 相互阻塞。
- HarmonyOS `VpnExtensionAbility`、用户 VPN 授权、隧道 socket `protect()` 与默认 IPv4 路由。
- `start --mtu 576...1500`：由 Mac 在握手中下发 MTU，设备 VPN 与 gVisor relay 使用同一值，默认 1400。
- Native C++ `PacketPump` 独占 TUN fd 与 Data socket，支持双向 raw IPv4 packet。
- Control 与 Data 各自每 5 秒发送 HNB 心跳；连续 15 秒未收到对应响应即关闭失效会话，避免保留黑洞默认路由。
- 设备在非主动中断后先停止 PacketPump、销毁旧 VPN，再以 1/2/4/8/10 秒上限退避重建整个会话；App 或 CLI 主动停止不会触发重连。
- daemon 非正常退出后会依据受保护状态文件，只回收本实例上次记录的精确 hdc 映射并恢复服务，不批量删除其他转发规则。
- Mac gVisor relay 支持 TCP、UDP 和 DNS-over-UDP / DNS-over-TCP。
- DNS 虚拟地址 `198.18.0.1`；Mac 端读取 `scutil --dns`，按最长域名后缀选择企业 split-DNS resolver。resolver 失败时立即刷新配置，UDP 截断响应自动改用 TCP，不静默回退公共 DNS。
- `status` 展示 MTU、运行时长、双通道 RTT、重连次数以及包/字节/流聚合值；不展示地址、端口、packet payload 或 session token。
- App 内置 Phase 2 网络自检，通过虚拟 DNS 分别执行 UDP 和 TCP 查询，验证真实的 TUN → USB → Mac → resolver 往返。
- Gate V 探针仍保留，便于在新设备上单独验证 VPN 授权、`protect()`、TUN read 与销毁。

协议细节见 [HNB/1](docs/protocol.md)，Phase 3 实现与验收见 [Phase 3 文档](docs/phase-3.md)，Phase 2 基线见 [Phase 2 文档](docs/phase-2.md)，原始能力分析见 [技术方案设计文档](docs/spark/2026-08-06-harmony-netbridge-design.md)。

## 架构

```text
HarmonyOS NEXT Apps / System IPv4 traffic
                    │
                    ▼
          VpnExtensionAbility
                    │
             TUN fd (MTU 576...1500)
                    │
          Native C++ PacketPump
                    │ HNB/1 IP_PACKET (Data TCP)
                    │
ArkTS Control TCP ──┤ 127.0.0.1:27183
                    │
                    │ hdc rport over USB
                    ▼
       Mac daemon 127.0.0.1:<dynamic>
                    │
            gVisor relay.Engine
              ┌─────┴─────┐
              │           │
       TCP/UDP sockets   macOS DNS resolvers
              │           │
              └─────┬─────┘
                    ▼
       macOS route / AnyConnect / Wi-Fi
```

设备侧和 Mac listener 都只绑定 loopback。项目不创建 macOS utun、不修改 pf、不需要 root，也不自行实现 USB 协议。

## 环境要求

- Apple Silicon Mac
- Go 1.24 或更高版本
- DevEco Studio 与 HarmonyOS SDK
- 已启用 USB 调试并授权的 HarmonyOS NEXT 真机
- 可用的 `hdc`

`hdc` 查找顺序：

1. `--hdc <path>`
2. `HARMONY_NETBRIDGE_HDC`
3. `PATH`
4. DevEco Studio 默认 SDK 路径

## 构建

Mac CLI：

```bash
./scripts/build-macos.sh
./bin/harmony-netbridge --version
```

HarmonyOS App：

```bash
./scripts/build-harmony.sh
```

未配置签名时会生成 `entry-default-unsigned.hap`；配置个人调试签名后会生成并优先报告 `entry-default-signed.hap`。真机必须安装与你设备匹配的签名 HAP。签名证书、密码和本机路径属于开发者本地配置，不应作为项目发布凭据共享。

## 真机使用

1. 连接并解锁手机，确认只有一个设备处于 `Connected` 状态。
2. 安装签名 HAP，然后启动 Mac daemon：

   ```bash
   hdc install -r harmony/HarmonyNetBridge/entry/build/default/outputs/default/entry-default-signed.hap
   ./bin/harmony-netbridge --mtu 1400 start
   ```

3. 打开 HarmonyNetBridge App，点击“启动 VPN”。首次使用时确认 HarmonyOS 的 VPN 授权提示。
4. 查看 Mac 状态：

   ```bash
   ./bin/harmony-netbridge status
   ```

   成功时应看到：

   ```text
   Daemon:    RUNNING
   Transport: DATA_CONNECTED
   VPN:       ACTIVE
   MTU:       1400
   Heartbeat: control 1 ms / data 1 ms
   Message:   IPv4 TCP, UDP, and DNS relay is active
   ```

5. 在 App 中点击“运行 TCP / UDP / DNS 自检”。`PASS` 表示 UDP DNS 和 TCP DNS 都已通过真实 VPN 数据面返回。
6. 停止时运行：

   ```bash
   ./bin/harmony-netbridge stop
   ```

   daemon 会先向 App 发送 `STOP_REQUEST`；设备停止 PacketPump、销毁 VPN 后，Mac 只删除本实例创建的精确 hdc 映射。

本阶段只运行一个设备会话。若 hdc 同时列出多个设备，必须显式选择本次唯一目标：

```bash
./bin/harmony-netbridge --device <hdc-target> start
```

完整设备 ID 只用于 hdc 目标参数，不写入默认结构化日志。

## 验证

全量静态、单元与 Host 集成检查：

```bash
./scripts/check.sh
```

脚本覆盖：

- Go race tests、`go vet`、macOS arm64 构建。
- HNB/1 parser、Control/Data 会话关联、daemon 生命周期和 hdc 映射清理。
- MTU 参数边界、双通道心跳、心跳 RTT 状态与单设备重连计数。
- gVisor 内存网络中的真实 TCP、UDP、UDP DNS 与 TCP DNS 往返。
- macOS resolver 失效刷新与 UDP DNS 截断后的 TCP 重试。
- ArkTS 协议、MTU、重连退避与 DNS TCP 分片测试，Native CMake/Ninja 构建和 HAP 打包。

这些检查不能替代真机证据。2026-08-06 已在单台物理设备完成 MTU 1280、TUN 双向流量、TCP/UDP/DNS 自检、双通道心跳、两轮 daemon 强制退出后的自动恢复，以及最终 VPN/进程/hdc 映射可靠清理；详细结果见 [Phase 3 文档](docs/phase-3.md)。跨小时、休眠唤醒和吞吐基准仍未执行。

## 当前限制

- 当前仅支持 IPv4 TCP/UDP/DNS；IPv6、ICMP 和其他原始协议尚未实现。
- 一次 daemon 只服务一个显式选择的设备；本阶段不实现 `devices` 命令或多设备并发。
- 自动重连只恢复 HarmonyNetBridge 自己的 Control/Data/VPN 资源，不能恢复物理 USB 断开、设备关机或被企业策略撤销的 VPN 权限；条件恢复后会继续退避重试。
- 尚未完成跨小时压力、休眠唤醒和高吞吐性能基准。
- hdc 调试授权是前提，本项目定位为开发工具，不是消费者 USB 网络共享产品。
- 同一时间只能存在一个活动 VPN；其他 VPN 或企业设备策略可能阻止启动。
- DNS 转发依赖 Mac 提供可用的 IPv4 resolver；不会为了“看起来可用”而绕过企业 DNS 使用公共服务器。
- Phase 4 的 mitmproxy / Charles 一键模式和证书引导尚未实现。

## 日志与隐私

- 日志：`~/Library/Logs/HarmonyNetBridge/harmony-netbridge.log`
- 运行目录：当前用户缓存目录下的 `HarmonyNetBridge/runtime`
- 日志最多保留当前文件与一个 5 MiB 轮转文件。

项目不记录 packet payload、完整 session token、完整设备唯一标识、签名凭据或代理凭据。

## 下一阶段

Phase 4 将实现 mitmproxy / Charles 一键抓包体验；多设备并发不在当前计划内。Phase 3 后续只继续补充跨小时稳定性、Mac 休眠唤醒与吞吐基准。

## License

[Apache License 2.0](LICENSE)
