# HNB/1 Protocol

HNB/1 是 HarmonyNetBridge 的有界二进制帧协议。Phase 1 只启用 Control 连接的 `HELLO`、`HELLO_ACK`、`ERROR` 和 `STOP`；Data 与 IP packet 类型为后续阶段保留。

## 固定头部

所有整数采用网络字节序。头部固定为 16 字节：

| Offset | Size | Field |
| ---: | ---: | --- |
| 0 | 4 | ASCII magic `HNB1` |
| 4 | 1 | version，当前为 `1` |
| 5 | 1 | frame type |
| 6 | 2 | flags，v1 必须为 `0` |
| 8 | 4 | payload length |
| 12 | 4 | sequence |

每条 TCP 连接的 sequence 从 `1` 开始，`0` 保留；到达 `uint32` 最大值后回到 `1`。sequence 在 v1 只用于诊断，不表示重传。

## Frame types

| Value | Name | Phase |
| ---: | --- | --- |
| `0x01` | `HELLO` | 1 |
| `0x02` | `HELLO_ACK` | 1 |
| `0x03` | `ERROR` | 1 |
| `0x04` | `STOP_REQUEST` | 1 |
| `0x05` | `STOP_ACK` | 1 |
| `0x10` | `DATA_HELLO` | 2 reserved |
| `0x11` | `DATA_ACK` | 2 reserved |
| `0x20` | `IP_PACKET` | 2 reserved |
| `0x30` | `PING` | 3 reserved |
| `0x31` | `PONG` | 3 reserved |

控制 payload 是 UTF-8 JSON，最大 16 KiB。`IP_PACKET` 将使用原始二进制，最大 65,535 字节。解析器在读取 payload 前验证 magic、version、type、flags、sequence 和长度；不会扫描任意字节尝试重新同步。

## Phase 1 handshake

App 必须在建立 TCP 连接后 5 秒内发送 sequence 1 的 `HELLO`：

```json
{
  "role": "control",
  "mode": "phase1",
  "appVersion": "development",
  "supportedVersions": [1],
  "capabilities": ["control"],
  "message": "hello"
}
```

Mac 返回 sequence 1 的 `HELLO_ACK`：

```json
{
  "selectedVersion": 1,
  "sessionToken": "32-lowercase-hex-characters",
  "capabilities": ["control"],
  "message": "world"
}
```

session token 来自 16 个密码学安全随机字节，不写入日志。它在 Phase 1 只证明会话成功创建；Phase 2 才用于绑定独立的 Control/Data 连接。

## Shared golden frame

跨语言 canonical 数据位于 [`testdata/hnb1-phase1-hello.json`](../testdata/hnb1-phase1-hello.json)。Go 与 ArkTS 测试都验证完整 header、JSON 字段顺序、payload 长度和 frame hex。

## Stop

Mac 发送 `STOP_REQUEST`：

```json
{"reason":"user_requested"}
```

App 返回无 payload 的 `STOP_ACK`，随后双方关闭连接。关闭和映射清理必须幂等。

## Error

```json
{
  "code": "VERSION_UNSUPPORTED",
  "message": "safe user-facing message",
  "fatal": true
}
```

能安全发送 `ERROR` 时先发送，再关闭连接。协议错误文本不得包含完整设备 ID、token、凭据或 packet 内容。
