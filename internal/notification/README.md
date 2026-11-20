# Notification 模块

本包负责 MoviePilot V2 `2.15.6` Telegram 出站投递。业务历史、持久投递状态和 V2 HTTP 投影分别属于 `notificationhistory`、`notificationflow`、`database` 与 `httpapi`，本包不保存业务事实。

## 当前边界

- 支持 V2 `Notifications` 列表中的多个已启用 Telegram 配置。
- 按 `channel`、`source`、`switchs` 和 `userid` 选择目标。
- 使用不可变 Registry 快照热重载运行态；发送只读取一个完整快照。
- 支持 V2 MarkdownV2/HTML、图片与文本、1024/4096 长度限制、链接预览三态和有界即时重试。
- 不实现通用出站 Webhook，也不把占位的 WebPush 注册到生产运行态。
- Telegram 入站命令、按钮回调、消息编辑、文件/语音发送以及 V2 其他通知渠道属于后续独立兼容切片。

## 生产数据流

```text
POST /api/v1/system/setting/Notifications
  -> PostgreSQL 加密保存
  -> notificationconfig.Decode
  -> Registry.Replace 原子发布

业务完成事务
  -> notification_event（每个媒体/季成功分组一条，失败文件各一条 Manual）
  -> notifications（每个 Telegram 配置一条投递意图）
  -> River notification worker
  -> notificationflow.Service
  -> Registry
  -> Telegram Bot API
```

没有匹配 Telegram 目标时，业务历史仍然保留。无论是否存在 required Emby，新历史和投递都在本地整理完成事务中立即进入 `published`/`pending`；required Emby 不阻塞新任务订阅结算。`blocked -> pending` 及媒体确认对订阅的推进仅用于仍匹配原 current intent 的升级前遗留行。

文件终态支持 `completed` 与 `terminal_failed`。每个失败文件生成一条 `Manual`；媒体/季分组在全部文件终态且至少有一个成功时生成成功子集 `organizeSuccess`，全失败批次不生成成功历史，媒体索引和刮削只消费成功 ordinal。跨季事件按终态 ordinal 稳定排序，最后终态失败时保留 V2 的 `err_msg`，最后终态成功时为空。

## 配置

生产配置使用 V2 原有设置接口：

```json
[
  {
    "name": "Telegram",
    "type": "telegram",
    "config": {
      "TELEGRAM_TOKEN": "123456:token",
      "TELEGRAM_CHAT_ID": "123456789",
      "TELEGRAM_USERS": "",
      "TELEGRAM_ADMINS": "123456789",
      "API_URL": ""
    },
    "switchs": ["资源下载", "整理入库", "订阅"],
    "enabled": true
  }
]
```

`MOVIEPILOT_TELEGRAM_BOT_TOKEN`、`MOVIEPILOT_TELEGRAM_CHAT_ID` 和 `MOVIEPILOT_TELEGRAM_API_URL` 仅在数据库尚无 `Notifications` 行时作为旧 Go 开发配置兜底。数据库一旦保存该设置，`null` 或空列表也具有权威性，不再回退环境变量。

## 可靠性边界

- PostgreSQL 是通知历史和投递状态的事实源，Redis 不参与通知状态。
- 每个外部投递具有稳定意图、幂等键、租约、最多 5 次持久重试和周期恢复。
- Telegram 客户端内部重试受上下文和固定次数约束，不创建无界 goroutine。
- Bot Token、自定义 API URL 和上游响应正文不得进入错误、日志或指标。
- Telegram Bot API 没有客户端幂等键。“Telegram 已接收、响应丢失”或“发送成功、落库前崩溃”仍可能重复投递；代码只能缩小窗口，不能承诺 exactly-once。

## 验证

```powershell
go test ./internal/notification ./internal/notificationflow -count=1
go test ./internal/httpapi ./internal/app -count=1
$env:MOVIEPILOT_TEST_DATABASE_URL = "postgres://moviepilot:moviepilot@127.0.0.1:55432/moviepilot_test?sslmode=disable"
go test -tags=integration ./internal/database ./internal/organizationflow -count=1
```

完整兼容范围以原项目 v2.15.6 的通知模板语义为准。
