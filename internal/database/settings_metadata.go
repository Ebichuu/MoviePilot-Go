package database

// V2 把媒体源的密钥 / 域名存在系统设置里，而不是环境变量：
// `app/core/config.py` 里这两个键属于 CONFIG_WATCH，改完会重建 TMDB 客户端。
// 我们沿用同样的键名，前端「设置 → 系统」页写入的就是它们。

// TMDBAPIKeySettingKey 是 TMDB 密钥（v3 API Key 或 v4 Read Access Token）。
const TMDBAPIKeySettingKey = "TMDB_API_KEY"

// TMDBAPIDomainSettingKey 是 TMDB 接口域名，默认 api.themoviedb.org。
const TMDBAPIDomainSettingKey = "TMDB_API_DOMAIN"

// ProxyHostSettingKey 是外部请求的出口代理（V2 同名键，属 CONFIG_WATCH）：
// 形如 http://127.0.0.1:7890 或 socks5://127.0.0.1:1080，空表示直连。
// 本机实测 TMDB 可直连、api.bgm.tv 直连不通（经代理 1.4s 返回），
// 所以这个键是 Bangumi 列表能不能出数据的关键。
const ProxyHostSettingKey = "PROXY_HOST"
