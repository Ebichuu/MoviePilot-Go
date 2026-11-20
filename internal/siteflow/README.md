# siteflow

`siteflow` 负责复刻 MoviePilot V2 `2.15.6` 在站点用户数据快照保存成功后的副作用顺序。它不解析站点页面、不持久化用户数据快照，也不处理 HTTP 响应。

## 核心职责

- 按 `SiteRefreshed -> 未读消息 -> 低分享率预警` 的顺序执行后处理。
- 在外部投递前同步保存 V2 通知历史。
- 使用站点消息 `source` 做持久去重，并保留 V2 将该值继续传给通知渠道筛选的既有行为。
- 将通知渠道失败降级为日志，不改变已经成功的站点刷新结果。
- 通过可选的窄接口发布 `SiteRefreshed`，不依赖 HTTP、数据库实现、River 或 Redis。

## 依赖关系

- 输入模型来自 `internal/indexer`。
- 通知历史契约来自 `internal/notificationhistory`。
- 外部投递使用 `internal/notification.Sender`。
- `internal/app` 负责装配，`internal/httpapi` 只依赖消费方接口。

## 使用

```go
service, err := siteflow.NewService(historyStore, sender, publisher, logger)
if err != nil {
    return err
}
err = service.AfterSiteUserDataRefresh(ctx, siteID, siteName, siteURL, data)
```

调用方必须先成功保存站点用户数据快照。通知历史写入失败会返回错误；渠道投递失败只记录稳定日志。

## 文件

- `userdata.go`：生产编排和 V2 字段格式化。
- `userdata_test.go`：顺序、去重、边界和失败语义契约测试。
- `DESIGN.md`：设计依据、取舍和已知限制。
