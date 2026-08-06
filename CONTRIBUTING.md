# Contributing

感谢参与 HarmonyNetBridge。提交改动前请先阅读[技术方案](docs/spark/2026-08-06-harmony-netbridge-design.md)，并保持阶段边界清晰。

## 原则

- 不使用或假设 Android API。
- HarmonyOS 系统能力必须来自当前官方 SDK；实验性结论必须标注设备、系统版本与验证方式。
- 不把 Lite 代理桥描述成全局 VPN，也不把编译或 mock 测试描述成真机验证。
- 不记录完整设备 ID、session token、packet payload、证书私钥或代理凭据。
- hdc 清理必须针对本实例记录的完整 mapping tuple，禁止全局删除映射。
- 保持 Control、Data 和 VPN 生命周期所有权明确。

## 本地检查

```bash
./scripts/check.sh
```

涉及真机行为的 PR 应附上设备状态、实际命令、可脱敏日志和未覆盖场景。不要提交个人签名材料、`local.properties`、构建目录或 DevEco 工作区文件。
