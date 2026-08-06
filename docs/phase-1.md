# Phase 1 实现与验收

## 实现范围

Mac 端使用 Go 标准库实现：

- 无第三方 CLI 框架的 `start/status/stop`。
- hdc 查找、版本检查、在线设备解析、显式单设备选择。
- `127.0.0.1:0` 动态 listener 与 `tcp:27183 -> tcp:<dynamic>` 精确 `rport`。
- 用户级后台 supervisor、Unix 控制 socket、原子状态快照。
- JSON 结构化日志、5 MiB 单代轮转与设备标识脱敏。
- HNB/1 有界 parser、5 秒 HELLO 超时、随机 session token、断连状态回退。
- 幂等停止与只清理本实例映射。

HarmonyOS 端使用 ArkTS Stage 模型实现：

- `EntryAbility` 与 ArkUI 状态页。
- 使用 `@kit.NetworkKit` 的 loopback `TCPSocket`。
- 支持 TCP 分片与粘包的 HNB/1 decoder。
- 自动首次连接、手动重试、hello/world、ERROR 与 STOP。
- 注册官方 `vpn` Extension Ability 骨架，但不创建 VPN。

## 自动化测试

```bash
./scripts/check.sh
```

覆盖重点：

- HNB/1 分片、合并、非法 magic/version/flags/type/sequence、超长和截断 payload。
- Go/ArkTS 共享 golden HELLO frame。
- 密码学随机 token 格式与唯一性。
- hdc 多种列表格式、无设备、离线、多设备与显式选机。
- hdc 创建/删除的完整参数以及输出脱敏。
- daemon 真实 TCP hello/world、实时 status、stop 与精确 mapping cleanup。
- ArkTS Host 单测的 UTF-8、分片/粘包与非法 magic。
- `go vet` 与 macOS arm64 原生构建。
- HarmonyOS Debug unsigned HAP 构建。

## 真机验收步骤

1. 在 DevEco Studio 配置个人调试签名并安装 HarmonyNetBridge。
2. 运行 `hdc list targets -v`，确认目标为 `Connected`（旧版可能显示 `Online`）。
3. 运行 `./bin/harmony-netbridge start`，确认 `PORT_READY`。
4. 启动 App，确认 UI 显示“hello / world 成功”。
5. 运行 `./bin/harmony-netbridge status`，确认 `CONTROL_CONNECTED` 与 `VPN: STOPPED`。
6. 连续发送/解析多个控制帧，验证分片和粘包。
7. 保持控制连接 60 分钟，并覆盖前后台、锁屏与熄屏。
8. 拔除 USB，确认 Mac 回退为 `PORT_READY`，App 显示断开，不误报 Connected。
9. 运行 `./bin/harmony-netbridge stop`，通过 `hdc fport ls` 确认只删除本实例映射。

上述步骤需要真实 `Connected` 设备。编译成功、Host 单测和 mock hdc 都不能替代真机证据。

## 下一阶段

Phase 1 真机验收后先执行 Gate V：只创建保留测试网段的单主机路由，不配置默认路由和 DNS，验证第三方 VPN 授权、`protect()`、真实 TUN read 与 `destroy()`。Gate V 通过后再制定 Phase 2 实施计划；失败时保留 Lite 代理桥路线。
