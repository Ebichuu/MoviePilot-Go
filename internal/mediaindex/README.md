# mediaindex

`mediaindex` 实现“本地文件已整理”到“required 媒体服务器已唯一确认入库”的持久请求状态机。对于目标 episode 集已经正确建立的普通电影/电视剧，episode 和父订阅已在 qBittorrent 明确接受资源且 `AcceptDownloadIntent` 事务成功时结算，不等待整理或本模块；它只依赖消费方接口，不导入 PostgreSQL、River、Redis、HTTP API 或具体 Emby 客户端。

## 主要能力

- 使用稳定请求 ID 和 token-CAS 租约领取持久确认请求。
- 单次调用只执行一次刷新，或确认最多 16 个文件；处理期间每 30 秒续租，调用结束即停止心跳。
- 将正常扫描延迟、零匹配、歧义和媒体服务器暂不可用写入 `next_attempt_at`，不消耗 River 的有限重试。
- 查询媒体服务器前重新校验本地文件大小与 SHA-256，文件已变化时不会产生假确认。
- 按配置声明的 POSIX/Windows 语义比较远端路径；存在 Provider ID 时同时校验 Provider ID。
- 所有文件确认后，通过一个存储事务完成请求；仅对升级前仍匹配原 parent/current intent 的 `pending_index` 遗留行做条件推进。
- 周期 sweep 有界恢复过期租约和提交后漏入队的请求。

organization 对普通订阅的新状态不再改变 episode 或父订阅；只有 legacy `pending_organization/organizing/pending_index/indexed` 路径仍可能由整理和确认事务条件推进。这个结论不表示目标集建立已经完整兼容；洗版 `current_priority/episode_priority` 和优先级 `100` 的完成语义也尚未纳入普通订阅结算保证。

## 使用方式

```go
service, err := mediaindex.NewService(leaseStore, fileStore, completionStore, registry, localVerifier)
if err != nil {
    return err
}
if err := service.Handle(ctx, requestID); err != nil {
    return err
}
```

消费方需要实现以下小接口：

- `LeaseStore`：领取、续租、延期和记录刷新。
- `FileStore`：分页读取待确认文件并逐文件 CAS 确认。
- `CompletionStore`：全部确认后原子完成请求，并兼容性处理仍匹配的遗留 `pending_index` 行。
- `Registry`：按固化的服务器 ID、类型和端点指纹解析运行时客户端。
- `LocalVerifier`：按持久化大小和 SHA-256 重新校验本地文件。
- `SweepStore` 与 `Enqueuer`：恢复扫描和幂等入队。

## 公开模型

| 类型 | 用途 |
| --- | --- |
| `CompletionPlan` | 整理完成事务使用的 required 服务器和视频文件快照 |
| `Request` / `Lease` | Worker 所需的持久请求与租约投影 |
| `File` | 不可变目标文件及确认状态投影 |
| `Server` / `Registry` | 媒体服务器无关的刷新与查询边界 |
| `LocalVerifier` | 本地不可变文件快照的查询前复核边界 |
| `Service` | 执行一次有界刷新或确认动作 |
| `SweepHandler` | 恢复过期租约并重新入队到期请求 |

## 资源边界

- 单次文件页最多 16 条。
- sweep 单页最多 100 个请求。
- 工作租约为 2 分钟，处理期间每 30 秒续租；续租失败立即取消本次外部调用。
- 稳定错误只保存分类，不保存 Token、完整 URL、本地路径或上游响应体。
- 远端路径比较不使用当前 Go 主机的 `filepath` 语义。

详细决策见 [DESIGN.md](DESIGN.md)。
