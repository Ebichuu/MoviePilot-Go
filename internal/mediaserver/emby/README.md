# emby

`emby` 是一个最小、强类型且资源有界的 Emby HTTP 客户端。它服务于媒体服务器运行时和入库确认业务，不承担配置持久化、订阅状态机或 V2 HTTP 转换。

## 主要能力

- `X-Emby-Token` 鉴权，可选安全的 Emby `Authorization` 客户端标识。
- 支持根路径或 `/emby` 子路径 Base URL，以及独立 `PlayHost`。
- 每个客户端最多 4 个在途请求和 4 条主机连接。
- GET 最多 3 次尝试，只重试临时传输错误及 429/502/503/504。
- 刷新 POST 只发送一次；响应丢失返回 `ErrOutcomeUnknown`。
- 只允许同源重定向，拒绝跨源、HTTPS 降级、方法改变和 `api_key` 注入。
- 响应体限制作用于解压后数据，错误体另设 64 KiB 上限。
- `FindItems` 支持有界分页：单页最多 200 条、起始位置最多 10000；唯一确认调用仍固定查询 2 条以发现歧义。
- 错误只包含稳定分类和状态码，不包含 Token、完整 URL、媒体路径或响应页。

## 示例

```go
client, err := emby.NewClient(emby.Config{
    BaseURL: "https://emby.example/emby",
    Token:   apiKey,
    Timeout: 10 * time.Second,
})
if err != nil {
    return err
}
defer client.CloseIdleConnections()

items, err := client.FindItems(ctx, emby.FindItemsOptions{
    Path: "/media/Movie.mkv",
    Recursive: true,
    Fields: []string{"Path", "ProviderIds"},
    Limit: 2,
})
```

## 公开方法

- `SystemInfo`
- `ListLibraries`
- `FindItems`
- `RefreshItem`
- `RefreshAll`
- `PlayURL`
- `CloseIdleConnections`

详细决策见 [DESIGN.md](DESIGN.md)。
