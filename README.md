# HarmonyNetBridge

HarmonyNetBridge 是一个面向 HarmonyOS NEXT 开发者的开源 USB 网络桥项目。目标是在不依赖 Android API 的前提下，让鸿蒙设备复用 Mac 的网络连接。

当前仓库实现的是 **Phase 1**：Mac CLI、`hdc rport` 通道、HarmonyOS Stage 应用、`VpnExtensionAbility` 骨架，以及 HNB/1 `hello/world` 握手。它还不会接管系统流量，也不是可用的全局 VPN。

## 当前能力

- `harmony-netbridge start`：检测 hdc、选择唯一在线设备、启动用户级后台服务并建立反向端口映射。
- `harmony-netbridge status`：通过实时 Unix socket 分别显示 daemon、USB transport 和 VPN 状态。
- `harmony-netbridge stop`：通知 App 停止，并且只删除本次 daemon 创建的精确 hdc 映射。
- HarmonyOS NEXT App：连接设备回环地址 `127.0.0.1:27183`，通过 USB 完成 HNB/1 `HELLO` / `HELLO_ACK`。
- 已注册官方 `vpn` 类型的 `VpnExtensionAbility` 骨架；Phase 1 不申请 VPN 信任、不创建路由、不读取 TUN。

完整方案与能力边界见 [技术方案设计文档](docs/spark/2026-08-06-harmony-netbridge-design.md)，协议细节见 [HNB/1](docs/protocol.md)。

## 架构

```text
HarmonyNetBridge App
  127.0.0.1:27183
          │
          │ hdc rport over USB
          ▼
Mac daemon
  127.0.0.1:<dynamic-port>
          │
          ├── Unix control socket ← start/status/stop
          └── HNB/1 hello/world

VpnExtensionAbility: registered skeleton only (VPN remains STOPPED)
```

两端都只监听 loopback，项目不自行实现 USB 协议。Phase 1 允许开发者手动打开 App，`start` 在 listener 和 hdc 映射就绪后返回 `PORT_READY`，不会把“等待 App”误报为“已连接”。

## 环境要求

- Apple Silicon Mac
- Go 1.24 或更高版本
- DevEco Studio 与 HarmonyOS SDK
- 可用的 `hdc`，以及已授权 USB 调试的 HarmonyOS NEXT 真机

hdc 查找顺序：

1. `--hdc <path>`
2. `HARMONY_NETBRIDGE_HDC`
3. `PATH`
4. DevEco Studio 默认 SDK 路径

## 构建 Mac CLI

```bash
./scripts/build-macos.sh
./bin/harmony-netbridge --version
```

也可以直接运行：

```bash
go run ./cmd/harmony-netbridge status
```

## 构建 HarmonyOS App

```bash
./scripts/build-harmony.sh
```

构建产物默认是：

```text
harmony/HarmonyNetBridge/entry/build/default/outputs/default/entry-default-unsigned.hap
```

仓库不会提交签名证书或密码。真机安装前，请在 DevEco Studio 中打开 `harmony/HarmonyNetBridge`，为 bundle `io.github.realskyrin.harmonynetbridge` 配置个人调试签名，然后由 DevEco Studio 构建和运行。

## Phase 1 使用

1. 连接手机、解锁并授权 USB 调试。
2. 确认 `hdc list targets -v` 中恰好一个设备为 `Connected`（兼容部分旧版输出的 `Online`）。
3. 启动 Mac 端：

   ```bash
   ./bin/harmony-netbridge start
   ```

4. 在手机上打开 HarmonyNetBridge App。App 会自动尝试连接一次，也可以点击“连接 Mac”。
5. 查看实时状态：

   ```bash
   ./bin/harmony-netbridge status
   ```

握手成功时应显示：

```text
Daemon:    RUNNING
Device:    Harmony device (device-xxxxxxxx)
Transport: CONTROL_CONNECTED
VPN:       STOPPED
Message:   Phase 1 handshake completed (hello/world)
```

停止服务：

```bash
./bin/harmony-netbridge stop
```

多台设备同时在线时必须显式选择一个目标：

```bash
./bin/harmony-netbridge --device <hdc-target> start
```

完整设备 ID 只作为 hdc 目标参数使用，不写入默认状态与结构化日志。

## 验证

```bash
./scripts/check.sh
```

也可以分别运行：

```bash
go test ./...
go vet ./...
GOOS=darwin GOARCH=arm64 go build ./cmd/harmony-netbridge
./scripts/test-harmony.sh
```

`test-harmony.sh` 会额外解析 Hypium 结果文件，因为当前 Hvigor Host 单测任务即使存在失败用例也可能返回退出码 0。

编译、mock 集成测试和 Host 单测都不等于真机 USB 证据。Phase 1 只有在一个 `Connected` 真机完成安装、运行与真实 hello/world 后才算完全验收。

## 日志与运行数据

- 日志：`~/Library/Logs/HarmonyNetBridge/harmony-netbridge.log`
- 运行目录：当前用户缓存目录下的 `HarmonyNetBridge/runtime`
- 日志最多保留当前文件与一个 5 MiB 轮转文件。

项目不会记录 packet payload、完整 session token、完整设备唯一标识、证书私钥或代理凭据。

## Roadmap

- Gate V：在真机上验证第三方 VPN 授权、`protect()`、受限路由、真实 TUN read 与可靠销毁。
- HarmonyNetBridge Lite：按 App 显式代理的 USB HTTP 代理桥。
- Phase 2：在 Gate V 通过后实现 TCP、UDP、DNS 用户态转发。
- Phase 3：MTU、DNS 优化、重连、心跳与多设备。
- Phase 4：mitmproxy / Charles 开发者抓包体验。

## License

[Apache License 2.0](LICENSE)
