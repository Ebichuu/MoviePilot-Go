# mediaconfig

`mediaconfig` 定义媒体服务器设置的领域模型、规范化、校验、密钥合并/脱敏和跨平台路径映射。配置的数据库加密由 `internal/database` 适配器负责，本包不依赖数据库或 HTTP。

## 主要能力

- 规范化并校验 Emby `host`、`play_host`、名称、稳定 UUID 和启用状态。
- 名称按 UTF-8 字节限制为 255，API Key 限制为 4096 字节，避免先保存后在数据库或客户端边界失败。
- 保证名称忽略大小写唯一，并把第一个启用的 Emby 固化为 `Snapshot.RequiredID`。
- `MergeSecrets` 在前端提交空密钥时保留旧 API Key；`RedactSecrets` 为读取接口生成安全副本。
- 支持多个本地到 Emby 的路径映射，按最长本地根匹配。
- 显式支持 POSIX/Windows 分隔符及大小写策略，不使用宿主机 `filepath` 解释远端路径。
- `Snapshot`、`Server` 和映射切片均通过深拷贝隔离运行时快照。

## 示例

```go
snapshot, changed, err := mediaconfig.Normalize(servers)
if err != nil {
    return err
}
if err := mediaconfig.Validate(snapshot.Servers); err != nil {
    return err
}
mapped, found, err := mediaconfig.ResolvePath(snapshot.Servers[0].PathMapping, localTarget)
```

## 公开 API

| API | 用途 |
| --- | --- |
| `Decode` | 严格解析 MediaServers 设置 JSON，并拒绝未知字段和尾随 JSON 值 |
| `Normalize` | 生成稳定身份、规范化端点并计算 required ID |
| `Validate` | 校验服务器、URL、名称和路径映射 |
| `MergeSecrets` | 合并编辑请求与旧密钥 |
| `RedactSecrets` | 生成不含 API Key 的读取快照 |
| `MapPath` / `ResolvePath` | 按远端路径语义映射一个本地目标 |

详细决策见 [DESIGN.md](DESIGN.md)。
