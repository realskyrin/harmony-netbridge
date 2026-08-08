# HarmonyNetBridge

HarmonyNetBridge 是一个面向 HarmonyOS NEXT 开发者的开源 USB 网络桥。它不依赖 Android API，让鸿蒙设备通过 USB 与 `hdc` 复用 Mac 当前的网络路由，包括企业 Wi-Fi、Cisco AnyConnect 等开发环境。

当前仓库已实现 **Phase 4 单设备开发版**：HarmonyOS `VpnExtensionAbility` 支持按白名单或黑名单接管 App，并为实际进入 Tunnel 的 App 配置默认 IPv4 路由；Mac 端通过 gVisor Netstack 复用当前网络与企业 split DNS。`proxy` 模式还会安全托管一个 loopback-only mitmweb，把已分流 App 的常见 HTTP/HTTPS TCP 流量接入抓包链路。

## 当前能力

- `harmony-netbridge start/proxy/status/stop`：管理单设备 daemon、`hdc rport`、实时状态和安全停止。
- `proxy` 自动发现并启动 mitmweb，使用独立 `.mitm` capture、项目专属 CA 目录和 loopback Web UI；停止 daemon 时只终止本次受管进程。
- `proxy --upstream <URL>` 可让 mitmweb 经指定 HTTP(S) upstream 出网；默认严格校验上游 TLS，开发调试时可显式传入 `--ssl-insecure`。
- 抓包模式通过 relay 内部 HTTP `CONNECT` 接入 mitmweb，不修改手机全局 HTTP proxy，也不依赖无法可靠回读的系统代理配置。
- HTTP 可直接抓取；App 可把 Mac 当前受管 `mitmproxy-ca-cert.cer` 经现有 hdc 通道保存到手机，并用系统证书管理器打开。最终安装与信任仍由用户在系统界面确认。
- 抓包模式拦截 TCP 80/443/8080/8443；UDP/443 被拒绝以促使 QUIC 回退 TCP，其他 UDP、DNS 和非 HTTP TCP 仍按标准模式转发。
- 独立的 HNB/1 Control/Data TCP 连接，以随机 session token 关联，避免控制帧与高频 packet 相互阻塞。
- HarmonyOS `VpnExtensionAbility`、用户 VPN 授权、隧道 socket `protect()` 与默认 IPv4 路由。
- App 设置页通过 Tab 切换白名单或黑名单模式，APP 分流卡片只展示已选 APP；点击“名单管理”后在二级 Bottom Sheet 中搜索和勾选完整已安装 APP 列表。HarmonyNetBridge 自身不显示在名单中并始终进入 Tunnel。两种模式共用同一份选中结果：白名单把本应用和选中 Bundle 写入 `VpnConfig.trustedApplications`，黑名单把选中 Bundle 写入 `blockedApplications`，且始终排除本应用。
- App 会以 0.5/1/2/4 秒上限退避探测 Mac 服务；先打开 App、随后启动 Mac Bridge，或重新插入数据线后，首页连接状态都会自动更新。VPN Tunnel 接管期间不会抢占连接。
- `start --mtu 576...1500`：由 Mac 在握手中下发 MTU，设备 VPN 与 gVisor relay 使用同一值，默认 1400。
- Native C++ `PacketPump` 独占 TUN fd 与 Data socket，支持双向 raw IPv4 packet。
- Control 与 Data 各自每 5 秒发送 HNB 心跳；连续 15 秒未收到对应响应即关闭失效会话，避免保留黑洞默认路由。
- 设备在非主动中断后先停止 PacketPump、销毁旧 VPN，再以 1/2/4/8/10 秒上限退避重建整个会话；App 或 CLI 主动停止不会触发重连。
- daemon 持续核对自己创建的精确 `hdc rport`；USB 数据线拔出导致映射丢失后，设备重新上线会自动补建同一映射，不必重启 Mac daemon。
- daemon 非正常退出后会依据受保护状态文件，只回收本实例上次记录的精确 hdc 映射并恢复服务，不批量删除其他转发规则。
- Mac gVisor relay 支持 TCP、UDP 和 DNS-over-UDP / DNS-over-TCP。
- DNS 虚拟地址 `198.18.0.1`；Mac 端读取 `scutil --dns`，按最长域名后缀选择企业 split-DNS resolver。resolver 失败时立即刷新配置，UDP 截断响应自动改用 TCP，不静默回退公共 DNS。
- `status` 展示 MTU、运行时长、双通道 RTT、重连次数以及包/字节/流聚合值；不展示地址、端口、packet payload 或 session token。
- App 内置 VPN 网络自检、Google/百度随机 HTTP 抓包自检，以及不依赖外部证书站点的 Mac CA 下载入口。
- Gate V 探针仍保留，便于在新设备上单独验证 VPN 授权、`protect()`、TUN read 与销毁。

协议细节见 [HNB/1](docs/protocol.md)，Phase 4 实现与验收见 [Phase 4 文档](docs/phase-4.md)，稳定性基线见 [Phase 3 文档](docs/phase-3.md)，原始能力分析见 [技术方案设计文档](docs/spark/2026-08-06-harmony-netbridge-design.md)。

## 架构

```text
HarmonyOS NEXT routed Apps IPv4 traffic
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
        ┌───────────┼──────────────┐
        │           │              │
 direct TCP/UDP   macOS DNS   proxy mode TCP
        │         resolvers    CONNECT 80/443/
        │           │          8080/8443
        │           │              │
        │           │           mitmweb
        └───────────┴──────┬───────┘
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
- Phase 4 抓包模式需要 `mitmweb`（mitmproxy 12.x 已验证）；标准 `start` 模式不需要

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

构建、安装并打开 App 供真机检查：

```bash
./scripts/run-ohos-app.sh
```

只有一台在线设备时脚本会自动选择；检测到多台设备时只列出统一的 1-based 序号并停止，使用 `./scripts/run-ohos-app.sh --device <序号>` 显式选择。可通过 `HNB_HDC` 覆盖 `hdc` 路径，也可通过 `HNB_DEVICE` 提供已明确选择的设备序号或 target。

## 真机使用

1. 连接并解锁手机，确认只有一个设备处于 `Connected` 状态。
2. 安装签名 HAP，然后启动 Mac daemon：

   ```bash
   hdc install -r harmony/HarmonyNetBridge/entry/build/default/outputs/default/entry-default-signed.hap
   ./bin/harmony-netbridge --mtu 1400 start
   ```

3. 打开 HarmonyNetBridge App，进入“设置 → APP 分流”，选择白名单或黑名单模式，点击“名单管理”，在 Bottom Sheet 中搜索并勾选应用，然后开启 VPN Tunnel。首次使用时确认 HarmonyOS 的 VPN 授权提示。
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

5. HarmonyNetBridge 自身不出现在名单中，并会在白名单和黑名单模式下始终进入 Tunnel，因此可直接运行 App 内置网络/抓包自检。`PASS` 表示 UDP DNS 和 TCP DNS 都已通过真实 VPN 数据面返回。
6. 停止时运行：

   ```bash
   ./bin/harmony-netbridge stop
   ```

   daemon 会先向 App 发送 `STOP_REQUEST`；设备停止 PacketPump、销毁 VPN 后，Mac 只删除本实例创建的精确 hdc 映射。

### APP 白名单与黑名单分流

- APP 分流卡片始终只展示已选 APP，不在设置页平铺完整安装列表；点击“名单管理”后，二级 Bottom Sheet 才展示搜索和完整已安装 APP 列表，列表支持下拉刷新，已选 APP 自动排在未选 APP 前面。HarmonyNetBridge 自身会从已安装列表和历史选中结果中移除，并作为隐式规则始终进入 Tunnel。白名单与黑名单 Tab 共用同一份选中结果，切换模式不会复制、清空或替换已选 APP。
- daemon 优先执行只读 `hdc shell bm dump -a -l` 获取本地化 APP 名称和 Bundle 名称；设备版本不支持标签输出时回退到 `bm dump -a`。列表请求必须携带当前控制会话的随机 token，连接断开后立即失效。
- 白名单模式让本应用和选中 APP 进入 Tunnel，对应 Bundle 写入 `VpnConfig.trustedApplications`；黑名单模式让选中 APP 保持直连，其余 APP 进入 Tunnel，对应 Bundle 写入 `blockedApplications`，本应用不会被写入黑名单。两种模式不会同时下发非空列表。
- 白名单为空时仍可创建 Tunnel，但只接管作为隐式白名单成员的 HarmonyNetBridge；黑名单为空表示不排除任何 APP，因此所有 APP 都会进入 Tunnel。
- 默认和旧版本迁移结果均为白名单模式，原有已选 APP 保持不变。Tunnel 活动期间不能修改模式或列表；先关闭 Tunnel，修改后再次开启即可使用新配置。
- 普通三方手机应用无权直接枚举完整已安装 APP 列表，因此 App 不申请系统级包管理权限；枚举动作由已选设备对应的本机 hdc 完成。

### 抓包模式

安装 mitmproxy 后，将第 2 步的 `start` 替换为：

```bash
./bin/harmony-netbridge --mtu 1400 proxy
```

命令会启动受管 mitmweb，并由 mitmweb 打开带一次性认证信息的 loopback Web UI。若不希望自动打开浏览器：

```bash
./bin/harmony-netbridge proxy --no-open-browser
```

若本机通过监听 `127.0.0.1:3128` 的代理访问外网，可将它配置为 mitmweb 的 upstream：

```bash
./bin/harmony-netbridge proxy --upstream http://127.0.0.1:3128
```

`--upstream` 只接受不含账号密码、路径、查询参数或 fragment 的 `http://`/`https://` URL，避免凭据出现在受管子进程命令行中。

若企业 upstream 替换 HTTPS 证书，而 mitmweb 的 Python 信任库无法构建该证书链，可在本次开发调试中显式关闭 mitmweb 的上游 TLS 校验：

```bash
./bin/harmony-netbridge proxy --upstream http://127.0.0.1:3128 --ssl-insecure
```

该开关不会替代手机对 mitmproxy CA 的信任，只影响 mitmweb 到 upstream/目标服务器的 TLS 身份校验。默认仍严格校验；启用后 `status` 会明确显示 `TLS verify: DISABLED`。

随后在 App 开启 VPN Tunnel，再点击“验证 HTTP 抓包链路”。HTTP 可立即在 mitmweb 中查看。HTTPS 请点击“下载 Mac CA 证书”，下载完成后点击“用证书管理器打开”，再由 HarmonyOS 系统界面确认安装与信任；不再需要去外部站点下载。App pinning、企业策略或不信任用户 CA 的应用仍可能拒绝解密，这不是隧道故障。

`status` 会显示代理状态、capture 文件、公共 CA 路径、已接入代理的 TCP flow 数和 QUIC 回退计数，但不会显示 mitmweb token、请求头或 flow 内容。默认文件位于：

- capture：`~/Library/Caches/HarmonyNetBridge/captures/*.mitm`
- CA 配置：`~/Library/Caches/HarmonyNetBridge/mitmproxy/`
- mitmweb 日志：`~/Library/Logs/HarmonyNetBridge/mitmweb.log`

capture、日志与 CA 文件使用 `0600`，所属目录使用 `0700`。停止命令不会删除 capture，便于之后用 `mitmdump --no-server --rfile <capture>` 离线分析。

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
- HNB/1 parser、Control/Data 会话关联、daemon 生命周期、hdc 映射清理，以及已安装 APP 标签/Bundle 列表解析与回退。
- MTU 参数边界、双通道心跳、心跳 RTT 状态与单设备重连计数。
- gVisor 内存网络中的真实 TCP、UDP、UDP DNS 与 TCP DNS 往返。
- HTTP CONNECT 适配、非 2xx/超长响应拒绝、buffered tunnel 数据保留、代理 flow 统计与 UDP/443 回退。
- mitmweb upstream 参数、TLS 校验开关、受管生命周期、私有 capture/CA 权限、两个只读 CA 下载端点和精确 orphan 识别。
- macOS resolver 失效刷新与 UDP DNS 截断后的 TCP 重试。
- ArkTS 协议、MTU、重连退避、DNS TCP 分片、已安装 APP 列表解码/搜索、CA 下载响应边界、随机 HTTP 检测目标与响应判定测试，Native CMake/Ninja 构建和 HAP 打包。

这些检查不能替代真机证据。2026-08-06 已在单台物理设备完成 Phase 3 的 MTU、双向流量、心跳和故障恢复验收；Phase 4 的当前真机与 mitmweb 证据见 [Phase 4 文档](docs/phase-4.md)。跨小时、休眠唤醒和吞吐基准仍未执行。

## 当前限制

- 当前仅支持 IPv4 TCP/UDP/DNS；IPv6、ICMP 和其他原始协议尚未实现。
- 一次 daemon 只服务一个显式选择的设备；本阶段不实现 `devices` 命令或多设备并发。
- 自动重连只恢复 HarmonyNetBridge 自己的 Control/Data/VPN 资源，不能恢复物理 USB 断开、设备关机或被企业策略撤销的 VPN 权限；条件恢复后会继续退避重试。
- 尚未完成跨小时压力、休眠唤醒和高吞吐性能基准。
- hdc 调试授权是前提，本项目定位为开发工具，不是消费者 USB 网络共享产品。
- 同一时间只能存在一个活动 VPN；其他 VPN 或企业设备策略可能阻止启动。
- DNS 转发依赖 Mac 提供可用的 IPv4 resolver；不会为了“看起来可用”而绕过企业 DNS 使用公共服务器。
- 代理模式只接管常见 HTTP(S) TCP 端口；它不是任意 TCP 协议解码器。WebSocket/HTTP2 由 mitmproxy 能力决定，HTTP/3 不直接抓取，而是通过拒绝 UDP/443 促使客户端回退。
- 当前自动托管的是 mitmweb；Charles 可手动作为未来 adapter 接入，但尚无受管生命周期实现。
- HarmonyNetBridge 不绕过证书校验。HTTPS 需要用户手动信任 CA；证书 pinning、企业安全策略或明确禁用用户 CA 的应用无法解密。

## 日志与隐私

- 日志：`~/Library/Logs/HarmonyNetBridge/harmony-netbridge.log`
- mitmweb 日志：`~/Library/Logs/HarmonyNetBridge/mitmweb.log`（可能含 mitmweb 自己生成的本机 UI 认证 URL，权限固定为 `0600`）
- 运行目录：当前用户缓存目录下的 `HarmonyNetBridge/runtime`
- 日志最多保留当前文件与一个 5 MiB 轮转文件。

项目不记录 packet payload、完整 session token、完整设备唯一标识、签名凭据或代理凭据。

## 后续计划

Phase 1—4 的单设备 MVP 已形成完整链路。后续优先补充跨小时稳定性、Mac 休眠唤醒、吞吐基准与 CA 安装的不同 HarmonyOS 版本兼容性；多设备并发不在当前计划内。

## License

[Apache License 2.0](LICENSE)
