# HNB/1 Protocol

HNB/1 是 HarmonyNetBridge 在 hdc TCP 通道上使用的有界二进制帧协议。Phase 3/4 使用一条低频 Control 连接和一条高频 Data 连接；二者由随机 session token 关联，分别维护独立 sequence 和心跳状态。Phase 4 只增加能力协商，不改变帧头或协议版本。

## 固定头部

所有整数采用网络字节序，头部固定为 16 字节：

| Offset | Size | Field |
| ---: | ---: | --- |
| 0 | 4 | ASCII magic `HNB1` |
| 4 | 1 | version，当前为 `1` |
| 5 | 1 | frame type |
| 6 | 2 | flags，v1 必须为 `0` |
| 8 | 4 | payload length |
| 12 | 4 | sequence |

每条 TCP 连接、每个发送方向的 sequence 从 `1` 开始，`0` 保留；到达 `uint32` 最大值后回到 `1`。Control 与 Data 均要求同一发送方向的所有 frame type 共用连续 sequence，发现丢帧、重复帧或乱序后立即关闭会话。

解析器在分配 payload 前验证 magic、version、type、flags、sequence 和长度，不扫描任意字节尝试重新同步。

## Frame types

| Value | Name | Payload | 状态 |
| ---: | --- | --- | --- |
| `0x01` | `HELLO` | UTF-8 JSON | Phase 1/3 Control |
| `0x02` | `HELLO_ACK` | UTF-8 JSON | Phase 1/3 Control |
| `0x03` | `ERROR` | UTF-8 JSON | Control/Data |
| `0x04` | `STOP_REQUEST` | UTF-8 JSON | Control |
| `0x05` | `STOP_ACK` | empty | Control |
| `0x10` | `DATA_HELLO` | UTF-8 JSON | Data |
| `0x11` | `DATA_ACK` | empty | Data |
| `0x12` | `VPN_STATUS` | UTF-8 JSON | Control |
| `0x20` | `IP_PACKET` | raw IPv4 packet | Data |
| `0x30` | `PING` | 8-byte opaque nonce | Control/Data |
| `0x31` | `PONG` | exact echoed nonce | Control/Data |

控制 payload 最大 16 KiB。`IP_PACKET` 最大 65,535 字节；当前数据面只接受完整 IPv4 packet，不接受以太网帧、IPv6 packet 或分段的单个 HNB payload。

## Control handshake

### Phase 1

App 在建立 TCP 连接后 5 秒内发送 sequence 1：

```json
{
  "role": "control",
  "mode": "phase1",
  "appVersion": "0.4.0",
  "supportedVersions": [1],
  "capabilities": ["control"],
  "message": "hello"
}
```

Mac 返回 sequence 1 的 `HELLO_ACK`，完成 `hello/world`。

### Phase 3/4 VPN

VpnExtensionAbility 使用新的 Control socket，不复用 UI 的 Phase 1 socket：

```json
{
  "role": "control",
  "mode": "vpn",
  "appVersion": "0.4.0",
  "supportedVersions": [1],
  "capabilities": ["control", "data", "tcp", "udp", "dns", "heartbeat", "reconnect", "mtu", "proxy"],
  "message": "hello"
}
```

Mac 返回：

```json
{
  "selectedVersion": 1,
  "sessionToken": "32-lowercase-hex-characters",
  "capabilities": ["control", "data", "tcp", "udp", "dns", "heartbeat", "reconnect", "mtu", "proxy"],
  "mtu": 1400,
  "message": "world"
}
```

session token 来自 16 个密码学安全随机字节，仅在当前 Control 会话有效，不落日志或状态文件。`mtu` 必须是 `576...1500` 的整数；Harmony VPN 与当前会话的 Mac relay 必须使用相同值。

`proxy` 是可选 capability：App 声明它表示能够展示抓包状态，Mac 只在受管 mitmweb 已成功监听时返回它。App 不因缺少 `proxy` 拒绝标准 VPN；返回 `proxy` 时 App 将抓包模式写入进程内状态并展示手动 CA 引导。IP packet 仍使用相同 Data stream，代理选择只发生在 Mac relay 内部。

## Data handshake 与 packet stream

Native PacketPump 建立第二条 TCP 连接，在 5 秒内发送 sequence 1：

```json
{
  "sessionToken": "token-from-control-hello-ack",
  "role": "data"
}
```

Mac 只有在以下条件全部满足时才返回 sequence 1 的 `DATA_ACK`：

- token 格式有效且精确匹配当前 VPN Control 会话；
- Control 会话仍在线且 `mode` 为 `vpn`；
- 当前会话尚未绑定其他 Data 连接；
- daemon 未进入停止状态。

`DATA_ACK` 后，双方的下一个 frame 使用 sequence 2，之后 `IP_PACKET`、`PING`、`PONG` 共用同一连续序列。Harmony → Mac 的 `IP_PACKET` 来自 TUN read；Mac → Harmony 的 `IP_PACKET` 由 gVisor Netstack 生成并写回 TUN。

Control 和 Data 必须分离：`STOP`、生命周期状态和错误不得与高频 packet 共用发送锁或读取循环。

## VPN status

设备通过 Control 连接报告：

```json
{
  "state": "ACTIVE",
  "message": "tun_active"
}
```

允许的 state：`AUTH_REQUIRED`、`STARTING`、`RECONNECTING`、`ACTIVE`、`STOPPED`、`FAILED`。Mac 只把它映射为受限的用户状态文案，不直接显示设备传来的任意 message；Data 尚未连接时不能接受 `ACTIVE`。

## Heartbeat

完成 VPN Control 握手后，Mac 每 5 秒在 Control 连接发送一个 `PING`；VPN 进入 `ACTIVE` 后，Data 连接也独立发送。payload 是 8 字节不透明 nonce，对端必须原样放入 `PONG`，不能解析为主机端序整数。一个方向同一时刻最多有一个待确认心跳；15 秒仍未收到匹配 `PONG` 时关闭该连接。

Harmony Control 侧另有 20 秒入站 watchdog，Native Data 读取也有 20 秒上限。因此 Mac 进程卡死、hdc 半开连接和只坏一条 Data 通道都能结束黑洞 VPN。心跳 frame 与业务 frame 使用相同 writer 和 sequence，不允许并发写破坏帧边界。

## Stop 与失败处理

Mac 在 Control 连接发送：

```json
{"reason":"user_requested"}
```

App 返回空 payload 的 `STOP_ACK`。VpnExtensionAbility 的清理顺序为：

1. 停止 Native PacketPump；
2. 关闭 Data socket；
3. `destroy()` VPN；
4. 关闭 Control socket；
5. 结束 Extension。

设备端的 VPN `destroy()`、终态上报、Control close 与 `STOPPED` 事件发布都使用分步截止时间；单个 HarmonyOS Promise 未返回时，后续幂等清理仍会继续。App 停止按钮另有 6 秒 watchdog：若仍未收到终态，会调用系统 `stopVpnExtensionAbility()` 收束 Extension，并退出瞬时 `STOPPING` 状态。

daemon 发送 `STOP_REQUEST` 后会在有界时间内等待 `STOP_ACK`，再关闭 Control/Data 连接。这样正常停止不会与 socket close 竞态并被设备误判为 transport failure；超时后仍会强制清理本实例资源。

Control 连接或 Data 连接意外断开时，设备必须先停止 PacketPump 并销毁 VPN，避免保留一个无法转发的默认路由；随后按 1、2、4、8、10 秒上限退避，创建全新的 Control token、Data socket 与 VPN。`STOP_REQUEST`、App 停止按钮和系统销毁属于终止事件，不进入重连。

## Error

```json
{
  "code": "DATA_SESSION_REJECTED",
  "message": "safe user-facing message",
  "fatal": true
}
```

能够安全发送 `ERROR` 时先发送，再关闭连接。错误文本不得包含完整设备 ID、session token、凭据、DNS 名称或 packet 内容。

## Shared golden frame

跨语言 canonical Phase 1 数据位于 [`testdata/hnb1-phase1-hello.json`](../testdata/hnb1-phase1-hello.json)。Go 与 ArkTS 测试验证相同 header、JSON 字段顺序、payload 长度和 frame hex。Go daemon 集成测试另覆盖 Control/Data token 关联、双向 `IP_PACKET`、MTU 下发、两条心跳与单设备重连计数。
