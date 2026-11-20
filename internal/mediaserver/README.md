# mediaserver

`internal/mediaserver` 是媒体服务器条目的领域包，保存 MoviePilot V2 `mediaserveritem` 同步缓存的类型与消费者端口。

## 职责

- `Item` / `Query` / `ItemStore` / `ItemLookup`：缓存行、V2 `get_item_id` 查询输入与读写端口。
- `Item.Valid`：拒绝没有服务器、条目 ID、本轮同步时间戳或类型不在 `电影`/`电视剧` 的行。
- `Query.Empty`：既没有 TMDB ID 也没有标题时按 V2 返回普通未命中。

## 非职责

不包含 HTTP 客户端、SQL、River 注册、配置加密或 V2 响应转换。Emby 协议适配在 `internal/mediaserver/emby`，缓存写入在 `internal/database`，同步与快路径组合在 `internal/app`。
