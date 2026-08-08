# Phase 4 实现与验收

## 范围

Phase 4 在 Phase 3 单设备 IPv4 VPN 上增加受管 mitmweb 抓包，不修改手机全局代理，也不改变 HNB/1 帧格式。

当前 App 在这一数据面前增加了按 App 白名单/黑名单分流：设置页主卡片只展示已选 APP，点击“名单管理”后在二级 Bottom Sheet 中搜索和勾选完整已安装 APP 列表，已选 APP 会自动排在未选 APP 前面，两种模式共用同一份选中结果。HarmonyNetBridge 自身从名单中隐藏并始终进入 TUN：白名单把本应用和选中 Bundle 写入 `VpnConfig.trustedApplications`；黑名单把选中 Bundle 写入 `blockedApplications`，并强制从中排除本应用。空白名单只接管本应用，空黑名单接管全部 App。

已实现：

- `harmony-netbridge proxy`：检查 `mitmweb`、启动 loopback regular proxy 与 Web UI、创建独立 `.mitm` capture，并继续启动原有 VPN relay。
- 可配置 `--mitmweb`、`--proxy-port`、`--web-port`、`--upstream`、`--ssl-insecure`、`--no-open-browser` 与原有 `--mtu`。
- gVisor 收到 TCP 80/443/8080/8443 flow 后，Mac 侧先向 mitmweb 发送有界 HTTP `CONNECT`；成功后才桥接设备 TCP。其他 TCP、UDP 与 DNS 仍使用 Mac 直连。
- 代理模式拒绝 UDP/443，促使支持回退的客户端从 HTTP/3/QUIC 改用可抓取的 TCP。此行为不会伪装成已抓取 HTTP/3。
- HNB/1 `HELLO_ACK.capabilities` 增加可选 `proxy`；Harmony App 据此展示模式并提供 `http://mitm.it` HTTP 链路自检。
- daemon 在同一 hdc loopback 端口提供唯一只读路径 `/mitmproxy-ca-cert.cer`；仅代理模式可用，并在发送前验证文件是有界的 X.509 CA。其他 proxy 文件、私钥、capture 与 Web UI 信息均不可寻址。
- daemon 在同一端口提供 `/installed-apps.json`：优先以 `bm dump -a -l` 返回本地化 APP 名称与 Bundle 名称，不支持时回退 `bm dump -a`；接口只接受当前 HNB 控制会话的随机 Bearer token，不申请手机端系统级包管理权限。
- App 可将该公共 CA 直接保存到手机应用目录，再以 `general.cer-certificate` 文件类型和只读 URI 授权交给系统证书管理器；不请求 `MANAGE_VPN`，不静默安装证书，也不修改系统信任策略。
- daemon 正常停止会先停止手机 VPN，再终止本次启动的 mitmweb；异常退出后的下一次启动只在 PID 与完整受管参数均匹配时回收 orphan。

## 数据路径

```text
HarmonyOS Apps selected by routing mode
        │
        ▼
VpnExtensionAbility / TUN / PacketPump
        │ HNB/1 IP_PACKET over hdc USB
        ▼
Mac gVisor relay
        │
        ├─ DNS / UDP / other TCP ─────────► Mac network
        │
        └─ TCP 80/443/8080/8443
                 │ HTTP CONNECT
                 ▼
              mitmweb
                 │
                 ▼
             Mac network
```

证书下载不进入 mitmweb，也不经过互联网：App 连接设备侧 `127.0.0.1:27183`，hdc 将请求送到同一个 Mac daemon，daemon 只返回当前受管 proxy session 的 `~/Library/Caches/HarmonyNetBridge/mitmproxy/mitmproxy-ca-cert.cer`。

这里没有给手机设置 `network-cfg set http_proxy`。当前设备工具没有可靠的 read-back/backup 接口，盲目设置后无法保证恢复用户原值；完整 VPN 已能在 Mac relay 内选择代理，因此无需改变设备全局状态。

## CLI

```bash
./bin/harmony-netbridge proxy
./bin/harmony-netbridge proxy --mtu 1280 --proxy-port 9080 --web-port 9081
./bin/harmony-netbridge proxy --mitmweb /opt/homebrew/bin/mitmweb --no-open-browser
./bin/harmony-netbridge proxy --upstream http://127.0.0.1:3128
./bin/harmony-netbridge proxy --upstream http://127.0.0.1:3128 --ssl-insecure
./bin/harmony-netbridge status
./bin/harmony-netbridge stop
```

默认情况下由 mitmweb 打开包含其临时认证信息的浏览器页面。CLI、state 输出和普通 daemon 日志不复制该 token。`--no-open-browser` 适合自动化验收，此时 Web UI 仍只监听 loopback。

同一 daemon 不能在 `start` 与 `proxy` 间热切换；必须先 `stop`，确保 relay、VPN 和代理生命周期保持一致。

## 文件与隐私

| 类型 | 默认位置 | 权限 |
| --- | --- | --- |
| capture | `~/Library/Caches/HarmonyNetBridge/captures/*.mitm` | 文件 `0600`，目录 `0700` |
| CA/config | `~/Library/Caches/HarmonyNetBridge/mitmproxy/` | 文件 `0600`，目录 `0700` |
| mitmweb log | `~/Library/Logs/HarmonyNetBridge/mitmweb.log` | `0600` |
| daemon state | `~/Library/Caches/HarmonyNetBridge/runtime/state.json` | `0600` |

状态只保存受管进程恢复所需的 PID、可执行文件、端口、项目路径、无凭据 upstream URL 和 TLS 校验开关；不保存 Web UI token、代理凭据、请求头、目标地址、DNS 名称或 flow payload。capture 在停止后保留，由用户决定何时删除。

## HTTPS 信任边界

mitmproxy regular proxy 对 HTTPS 使用动态证书，因此设备必须信任该项目 confdir 生成的 CA。HarmonyNetBridge App 现在直接通过 USB 下载这一份公共 `.cer`，无需再从 `mitm.it` 获取证书。下载文件保存在 App 沙箱；点击安装按钮后，App 明确拉起系统证书管理器，并为该文件提供只读 URI 临时授权。

HarmonyOS SDK 26 的 CA 专用 `openInstallCertificateDialog` 在手机品类上声明为不支持，因此这里使用官方文件打开流程，而不伪装成第三方 App 可以直接写入系统信任区。系统证书管理器启动后，用户仍必须确认安装；取消、企业策略拒绝或设备不允许用户 CA 都不会被绕过。文件打开流程参考 [HarmonyOS 拉起文件处理类应用](https://developer.huawei.com/consumer/cn/doc/harmonyos-guides/file-processing-apps-startup)。

HarmonyOS 网络安全配置可允许应用信任用户 CA，但具体 App 可以关闭用户 CA、实施 certificate pinning，企业策略也可能禁止安装。HarmonyNetBridge 不绕过这些校验。参考：

Mac 到企业 upstream 是另一条独立的 TLS 信任链。若 mitmweb 的 Python 信任库无法识别企业拦截证书，可在开发调试时显式传入 `--ssl-insecure`；该选项只关闭 mitmweb 的上游证书校验，默认不启用，并由 `status` 明确显示。

- [mitmproxy Certificates](https://docs.mitmproxy.org/stable/concepts/certificates/)
- [mitmproxy Proxy Modes](https://docs.mitmproxy.org/stable/concepts/modes/)
- [OpenHarmony HTTP 请求与网络安全配置](https://gitee.com/openharmony/docs/blob/master/zh-cn/application-dev/network/http-request.md)

## 自动化验收

`./scripts/check.sh` 覆盖：

- Go race tests、`go vet` 与 Apple Silicon 构建。
- CONNECT request、非成功响应、32 KiB header 上限和同一 socket 中预读 tunnel payload。
- 被代理 TCP flow、UDP/443 阻断计数，以及其他 UDP/TCP 直连边界。
- daemon capability、代理生命周期、状态安全输出与精确 orphan 判定。
- CA 端点只允许 GET 固定路径、拒绝标准模式下载，并保持证书原始字节不变。
- ArkTS `proxy` capability、UI 状态、CA 响应边界、`mitm.it` 响应判定、Native 构建与签名 HAP 打包。

## 本机前置验收（2026-08-06）

在 Apple Silicon Mac、mitmproxy 12.2.3 上完成：

- `proxy --no-open-browser --mtu 1280` 启动成功，proxy/Web listener 均只绑定 `127.0.0.1`。
- 通过该代理请求 `http://mitm.it` 返回 HTTP 200，独立 capture 增长到非零大小。
- capture 为 `0600`，capture/CA 目录为 `0700`，CA 目录中没有权限宽于 `0600` 的文件。
- `stop` 后 loopback proxy 端口关闭、受管 mitmweb 进程计数为 0、项目 hdc 27183 映射计数为 0。

这证明了 Mac 代理进程、capture 与清理链路；设备 TUN 发起的 HTTP/HTTPS 仍需要下面的真机步骤单独证明。

## 本轮真机结果（2026-08-06）

以下结果记录的是 APP 分流功能加入前的 Phase 4 基线；它不构成当前白名单/黑名单分流的真机验证。当前版本复测时必须确保目标 App 会进入 Tunnel；HarmonyNetBridge 自身已作为隐式规则始终进入 Tunnel，可直接用于内置自检。

在一台已授权 USB 调试的 HarmonyOS NEXT 真机上安装 0.4.0 签名 HAP 后完成：

- 以 `proxy --no-open-browser --mtu 1280` 启动；App 与 CLI 均报告 `DATA_CONNECTED / ACTIVE / Proxy ACTIVE`，App 明确显示“Mac USB 抓包通道已接管”。
- App 的 `mitm.it` HTTP 自检为 `PASS`。首次 capture 增长 19,740 字节，CLI `proxied TCP` 从 1 增至 2；这是设备 TUN 发起而非 Mac curl 的数据面证据。
- 同一 VPN 中原有网络自检仍为 `PASS`：UDP DNS 1 条回答、TCP DNS 1 条回答，说明 Phase 4 没有破坏 UDP/DNS 直连路径。
- VPN 活动时精确强制终止 daemon：App 进入 `RECONNECTING`，旧 mitmweb 与旧 hdc mapping 均暂时存在。重新运行 `proxy` 后，daemon 只回收参数匹配的 orphan 和状态文件记录的旧映射，创建新 capture/host port，App 自动恢复 `ACTIVE`，`Reconnects` 为 1。
- 另行验证了崩溃后直接改用标准 `start`：旧 mitmweb 被精确回收、代理端口关闭，标准 daemon 保留且只保留 1 条新 hdc 映射；最终停止后映射与受管代理计数均为 0。
- 恢复后再次运行 HTTP 抓包自检，新 capture 增长 19,741 字节，`proxied TCP` 为 1，证明恢复了实际抓包数据面而不只是状态。
- 最终 `stop` 后 App 为 `STOPPED`，proxy/Web 两个 loopback 端口均关闭，受管 mitmweb 进程计数为 0，`tcp:27183` 项目映射计数为 0。
- 完成最后一次 UI 文案修正后重新构建并安装同一 0.4.0 签名 HAP；界面显示“mitmweb 已就绪”，HTTP 自检再次 `PASS`，capture 增长 19,742 字节，随后重复确认全部资源为停止态。
- 新增 CA 内置下载后，以最终签名 HAP 再次启动代理 VPN：App 点击“下载 Mac CA 证书”后显示已保存 `mitmproxy-ca-cert.cer`（1,172 字节），Mac daemon 同时记录发送 1,172 字节，证明证书来自当前受管 proxy session，而不是外部站点。
- 点击“用证书管理器打开”后直接进入系统 `com.ohos.certmanager` 的“证书详情”，文件名与 `mitmproxy` 证书信息被正确解析，系统“安装”按钮可见；验收没有点击安装。最终 daemon/VPN 为 `STOPPED`，受管 mitmweb、8080/8081 listener 与 `tcp:27183` hdc mapping 均为 0。

本轮只验证了 CA 下载与系统安装入口，没有替用户确认安装或信任，因此 HTTPS 解密在“用户手动安装 CA”之前保持未验证，不能由 HTTP `PASS` 或证书详情页推断为已经完成。

## 真机验收步骤

1. 安装 0.4.0 签名 HAP，运行 `./bin/harmony-netbridge proxy --no-open-browser --mtu 1280`。
2. 在 App 设置中选择分流模式，点击“名单管理”打开 APP 选择 Bottom Sheet 配置目标 App：白名单模式选中需要分流的其他 App，黑名单模式选中需要保持直连的 App。确认 HarmonyNetBridge 自身不出现在名单中；它会始终进入 Tunnel。关闭 Sheet 后主卡片应只显示已选 APP。开启 Tunnel，确认 UI 识别抓包模式，CLI 为 `DATA_CONNECTED / ACTIVE / Proxy ACTIVE`。
3. App 点击“验证 HTTP 抓包链路”；确认 `PASS`、CLI `proxied TCP` 增长且 capture 大小增加。
4. 在 mitmweb 中确认 `mitm.it` HTTP flow。不要把请求头或认证数据复制到验收日志。
5. 如需 HTTPS，在 App 点击“下载 Mac CA 证书”，成功后点击“用证书管理器打开”，在系统界面自行确认安装；未安装前的 TLS 信任失败属于预期边界。
6. 强制退出 daemon 一次，确认下一次 `proxy` 启动能精确回收 orphan mitmweb 与 hdc mapping，并恢复 VPN。
7. 执行 `stop`，确认 App `STOPPED`、mitmweb 不存在、两个 loopback 端口关闭、hdc 项目映射为 0。

## 当前限制

- 当前只支持 IPv4；IPv6、ICMP 与任意原始协议不在 Phase 4 范围。
- 自动托管 mitmweb，不自动启动 Charles。
- 只选择常见 HTTP(S) TCP 端口；其他端口默认直连。
- HTTP/3 依赖客户端支持 QUIC 失败后的 TCP 回退；强制只用 QUIC 的应用会失败而不是被解密。
- CA 安装与信任取决于 HarmonyOS 版本、目标 App 网络安全配置和设备管理策略。
- 尚未完成跨小时、休眠唤醒和吞吐基准。
