package httpapi

import (
	"net/http"
	"strconv"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/version"
)

// maxRestoreUpload 是恢复包的大小上限（512 MiB）。恢复包是数据库数据的压缩包，
// 普通部署远小于这个数；设上限是防止误传大文件把内存/磁盘拖死。
const maxRestoreUpload = 512 << 20

// systemBackup 对应扩展接口 `GET /system/backup`：把整库数据导出成 tar.gz 下载。
//
// 为什么 V2 没有而我们必须有：V2 用 SQLite，用户备份就是拷一个 moviepilot.db；
// 我们换成了 PostgreSQL，用户没法再"拷个文件就走"，没有导出入口等于没有退路，
// 升级前不敢动、迁移不了、出事没法回滚。这属于把产品做完整的一部分。
func (d Dependencies) systemBackup(w http.ResponseWriter, r *http.Request) {
	if d.DB == nil {
		writeError(w, http.StatusServiceUnavailable, "数据库未就绪")
		return
	}
	// 先探测临时目录：导出是流式响应，一旦写出字节就没法再改状态码，
	// 目录不可写必须在这里就拒绝，而不是给用户一个 10 字节的坏包。
	if _, err := database.BackupTempDir(); err != nil {
		writeError(w, http.StatusServiceUnavailable, "备份暂不可用: "+err.Error())
		return
	}
	filename := "moviepilot-backup-" + time.Now().Format("20060102-150405") + ".tar.gz"
	// 响应头必须在写入第一字节前设置：一旦开始流式输出就无法再改状态码，
	// 所以导出中途失败只能体现在日志里（下面的错误处理也是这个原因）。
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.Header().Set("Cache-Control", "no-store")
	manifest, err := database.BackupTo(r.Context(), d.DB, w, version.Version)
	if err != nil {
		if d.Logger != nil {
			d.Logger.Error("导出数据库备份失败", "error", err)
		}
		return
	}
	if d.Logger != nil {
		d.Logger.Info("已导出数据库备份",
			"file", filename, "tables", len(manifest.Tables), "schema_version", manifest.SchemaVersion)
	}
}

// systemRestore 对应扩展接口 `POST /system/restore`：上传 tar.gz 恢复整库数据。
//
// 恢复是破坏性操作：会清空现有业务表再写入包内数据（单事务，失败回滚）。
// 因此只允许管理员调用，并且要求客户端显式带 `?confirm=true`，
// 避免前端误触或者脚本手滑把库覆盖掉。
func (d Dependencies) systemRestore(w http.ResponseWriter, r *http.Request) {
	if d.DB == nil {
		writeError(w, http.StatusServiceUnavailable, "数据库未就绪")
		return
	}
	if !isTruthyQuery(r, "confirm") {
		writeError(w, http.StatusBadRequest, "恢复会覆盖现有数据，请带 confirm=true 确认")
		return
	}
	if r.ContentLength > maxRestoreUpload {
		writeError(w, http.StatusRequestEntityTooLarge,
			"备份包过大（上限 "+strconv.Itoa(maxRestoreUpload>>20)+" MiB）")
		return
	}
	body := http.MaxBytesReader(w, r.Body, maxRestoreUpload)
	manifest, err := database.RestoreFrom(r.Context(), d.DB, body)
	if err != nil {
		if d.Logger != nil {
			d.Logger.Error("恢复数据库备份失败", "error", err)
		}
		writeError(w, http.StatusBadRequest, "恢复失败: "+err.Error())
		return
	}
	if d.Logger != nil {
		d.Logger.Warn("已从备份恢复数据库",
			"tables", len(manifest.Tables), "schema_version", manifest.SchemaVersion,
			"created_at", manifest.CreatedAt.Format(time.RFC3339))
	}
	writeSuccess(w, map[string]any{
		"tables":         len(manifest.Tables),
		"schema_version": manifest.SchemaVersion,
		"created_at":     manifest.CreatedAt,
		"app_version":    manifest.AppVersion,
	})
}

// isTruthyQuery 解析 `1/true/yes/on` 这类布尔查询参数。
func isTruthyQuery(r *http.Request, key string) bool {
	switch r.URL.Query().Get(key) {
	case "1", "true", "TRUE", "True", "yes", "on":
		return true
	default:
		return false
	}
}
