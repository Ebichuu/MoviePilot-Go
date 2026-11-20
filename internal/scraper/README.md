# Scraper 模块

## 概述

Scraper 模块负责生成 NFO 元数据文件，供 Kodi、Emby、Jellyfin 等媒体服务器识别和刮削媒体信息。

## 架构

```
scraper/
├── nfo.go             # NFO 数据结构和 XML 生成
├── nfo_test.go        # NFO 生成测试
├── writer.go          # NFO 文件写入（原子操作）
├── writer_test.go     # 写入测试
├── service.go         # 协调服务（元数据获取 + NFO 生成）
├── service_test.go    # 服务测试
└── README.md
```

## 核心功能

### 1. NFO 数据结构

支持三种 NFO 类型，符合 Kodi/Emby 标准：

- **MovieNFO**：电影元数据
- **TVShowNFO**：剧集元数据（剧集根目录的 `tvshow.nfo`）
- **EpisodeNFO**：单集元数据

### 2. XML 生成

```go
meta := scraper.NFOMetadata{
    Type:          scraper.NFOTypeMovie,
    Title:         "Inception",
    Year:          2010,
    Rating:        8.8,
    Plot:          "A thief who steals corporate secrets...",
    Genres:        []string{"Action", "Sci-Fi"},
    TMDBID:        27205,
    IMDBID:        "tt1375666",
}

nfoData, err := scraper.GenerateNFO(meta)
// 返回标准 XML 格式的 NFO 内容
```

生成的 XML 示例：

```xml
<?xml version="1.0" encoding="UTF-8"?>
<movie>
  <title>Inception</title>
  <year>2010</year>
  <rating>8.8</rating>
  <plot>A thief who steals corporate secrets...</plot>
  <genre>Action</genre>
  <genre>Sci-Fi</genre>
  <uniqueid type="tmdb" default="true">27205</uniqueid>
  <uniqueid type="imdb">tt1375666</uniqueid>
</movie>
```

### 3. 文件写入

`NFOWriter` 负责将 NFO 内容写入文件系统，特点：

- **原子写入**：先写临时文件，再原子重命名，避免写入失败导致的半成品文件
- **自动路径推导**：根据媒体文件路径自动生成对应的 NFO 文件路径
- **安全验证**：路径必须是绝对路径，禁止路径遍历

```go
writer := scraper.NewNFOWriter()
err := writer.WriteNFOForMedia(ctx, "/media/Movie (2010)/Movie (2010).mkv", meta)
// 生成：/media/Movie (2010)/Movie (2010).nfo
```

### 4. 协调服务

`ScraperService` 协调元数据获取和 NFO 生成：

```go
service := scraper.NewScraperService(metadataProvider, writer, logger)

// 电影 NFO
err := service.ScrapeMovieNFO(ctx, tmdbID, "/media/Movie.mkv")

// 剧集 NFO
err := service.ScrapeTVShowNFO(ctx, tmdbID, "/media/Show/tvshow.nfo")

// 单集 NFO
err := service.ScrapeEpisodeNFO(ctx, tmdbID, season, episode, "/media/Show/S01E01.mp4")
```

## NFO 文件命名规则

| 媒体类型 | 媒体文件路径 | NFO 文件路径 |
|---------|-------------|-------------|
| 电影 | `/media/Movie (2010)/Movie (2010).mkv` | `/media/Movie (2010)/Movie (2010).nfo` |
| 剧集根 | `/media/Show/` | `/media/Show/tvshow.nfo` |
| 单集 | `/media/Show/Season 01/Show S01E01.mp4` | `/media/Show/Season 01/Show S01E01.nfo` |

## 元数据字段映射

### 电影 (MovieNFO)

| NFO 字段 | 说明 | 来源 |
|---------|------|------|
| `title` | 标题 | TMDB `title` |
| `originaltitle` | 原始标题 | TMDB `original_title` |
| `year` | 年份 | TMDB `release_date` 年份部分 |
| `rating` | 评分 | TMDB `vote_average` |
| `plot` | 剧情 | TMDB `overview` |
| `tagline` | 宣传语 | TMDB `tagline` |
| `runtime` | 时长（分钟） | TMDB `runtime` |
| `premiered` | 首映日期 | TMDB `release_date` (YYYY-MM-DD) |
| `genre` | 类型 | TMDB `genres[].name` |
| `director` | 导演 | TMDB `credits.crew` (job=Director) |
| `credits` | 编剧 | TMDB `credits.crew` (job=Writer) |
| `actor` | 演员 | TMDB `credits.cast` |
| `thumb` | 海报 | TMDB `poster_path` |
| `fanart` | 背景图 | TMDB `backdrop_path` |
| `uniqueid` | 外部 ID | TMDB ID (default), IMDB ID |

### 剧集 (TVShowNFO)

| NFO 字段 | 说明 | 来源 |
|---------|------|------|
| `title` | 标题 | TMDB `name` |
| `year` | 年份 | TMDB `first_air_date` 年份部分 |
| `rating` | 评分 | TMDB `vote_average` |
| `plot` | 剧情 | TMDB `overview` |
| `premiered` | 首播日期 | TMDB `first_air_date` |
| `genre` | 类型 | TMDB `genres[].name` |
| `studio` | 制片厂 | TMDB `networks[].name` |
| `uniqueid` | 外部 ID | TMDB ID, TVDB ID, IMDB ID |

### 单集 (EpisodeNFO)

| NFO 字段 | 说明 | 来源 |
|---------|------|------|
| `title` | 标题 | TMDB episode `name` |
| `season` | 季数 | 显式指定 |
| `episode` | 集数 | 显式指定 |
| `rating` | 评分 | TMDB episode `vote_average` |
| `plot` | 剧情 | TMDB episode `overview` |
| `aired` | 播出日期 | TMDB episode `air_date` |
| `runtime` | 时长 | TMDB episode `runtime` |
| `director` | 导演 | TMDB episode `crew` (job=Director) |
| `credits` | 编剧 | TMDB episode `crew` (job=Writer) |

## 测试

```bash
# 单元测试
go test ./internal/scraper/... -v

# 测试覆盖率
go test ./internal/scraper/... -cover
```

**测试覆盖**：
- NFO XML 生成（电影、剧集、单集）
- 最小元数据处理
- 不支持的类型错误处理
- 文件写入（原子操作、路径推导）
- 路径验证（绝对路径、遍历防护）
- 协调服务（成功流程、错误处理）

## 使用示例

### 完整流程

```go
import (
    "context"
    "log/slog"
    "github.com/moviepilot-go/moviepilot-go/internal/scraper"
)

// 1. 创建元数据提供器（假设已实现 TMDB 客户端）
provider := NewTMDBMetadataProvider(apiKey, cache)

// 2. 创建 NFO 写入器
writer := scraper.NewNFOWriter()

// 3. 创建协调服务
service := scraper.NewScraperService(provider, writer, slog.Default())

// 4. 为电影生成 NFO
ctx := context.Background()
err := service.ScrapeMovieNFO(ctx, 27205, "/media/Inception (2010)/Inception (2010).mkv")
if err != nil {
    log.Fatalf("Failed to scrape movie NFO: %v", err)
}
```

### 手动生成 NFO

```go
// 如果已有元数据，可以直接生成 NFO
meta := scraper.NFOMetadata{
    Type:      scraper.NFOTypeMovie,
    Title:     "Inception",
    Year:      2010,
    Rating:    8.8,
    Plot:      "A thief who steals corporate secrets through dream-sharing technology.",
    Genres:    []string{"Action", "Sci-Fi", "Thriller"},
    Directors: []string{"Christopher Nolan"},
    TMDBID:    27205,
    IMDBID:    "tt1375666",
}

// 生成 XML
nfoData, err := scraper.GenerateNFO(meta)

// 写入文件
writer := scraper.NewNFOWriter()
err = writer.WriteNFOForMedia(ctx, "/media/Movie.mkv", meta)
```

## 安全注意事项

### 路径验证
- ✅ 路径必须是绝对路径
- ✅ 禁止包含 `..` 路径遍历
- ✅ 自动清理路径分隔符

### 原子写入
- 先写入 `.nfo.tmp` 临时文件
- 写入成功后原子重命名为 `.nfo`
- 失败时自动清理临时文件

### XML 转义
- 自动转义特殊字符（`<`, `>`, `&`, `'`, `"`）
- 防止 XML 注入攻击

## 集成到整理流程

NFO 刮削将集成到 `organizationflow` 中，在文件整理完成后自动生成：

```
文件整理完成 → 媒体识别（TMDB ID）→ NFO 刮削 → 入库通知
```

配置项（未来）：
- `MOVIEPILOT_SCRAPER_ENABLED=true` - 启用 NFO 刮削
- `MOVIEPILOT_SCRAPER_LANGUAGE=zh-CN` - NFO 语言偏好
- `MOVIEPILOT_SCRAPER_INCLUDE_ACTORS=true` - 包含演员信息

## 扩展计划

- [ ] **图片刮削**：下载 poster、backdrop、actor thumb
- [ ] **多语言支持**：根据用户偏好选择元数据语言
- [ ] **自定义模板**：支持用户自定义 NFO 字段
- [ ] **批量刮削**：一次性刮削整个目录
- [ ] **增量更新**：仅刮削缺失或过期的 NFO
- [ ] **TVDB 支持**：除 TMDB 外增加 TVDB 元数据源

## 性能特性

- **并发安全**：多个 goroutine 可同时调用 `WriteNFOForMedia`
- **轻量级**：单个 NFO 文件通常 < 10KB
- **快速生成**：XML 序列化耗时 < 1ms
- **无阻塞**：NFO 生成失败不影响整理流程

## 兼容性

| 媒体服务器 | 兼容性 | 备注 |
|----------|--------|------|
| Kodi | ✅ 完全兼容 | 标准 NFO 格式 |
| Emby | ✅ 完全兼容 | 优先读取 NFO |
| Jellyfin | ✅ 完全兼容 | Fork 自 Emby |
| Plex | ⚠️ 部分兼容 | Plex 优先使用自己的元数据，NFO 作为备选 |

## 代码统计

- **总行数**：1404 行
- **测试覆盖**：21 个测试用例
- **文件数量**：7 个文件（3 个实现 + 3 个测试 + README）

## 参考资料

- [Kodi NFO Files Wiki](https://kodi.wiki/view/NFO_files)
- [Emby Metadata](https://support.emby.media/support/solutions/articles/44001159102-movie-naming)
- [TMDB API Documentation](https://developers.themoviedb.org/3)
