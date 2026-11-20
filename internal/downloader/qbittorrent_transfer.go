package downloader

import (
	"context"
	"errors"
)

var (
	errTransferStatsUnavailable = errors.New("下载器传输状态不可用")
	errTransferStatsQuery       = errors.New("下载器传输状态查询失败")
)

// TransferStats 汇总 qBittorrent 的全局速率、累计流量与下载盘剩余空间，供仪表盘
// 「下载器」卡片展示。V2 用 `DashboardChain().downloader_info` 得到同一组数值。
func (q *QBittorrent) TransferStats(ctx context.Context) (TransferStats, error) {
	if ctx == nil || q == nil || q.client == nil || q.queryPermit == nil {
		return TransferStats{}, errTransferStatsUnavailable
	}
	var stats TransferStats
	err := q.withQueryPermit(ctx, func() error {
		if err := q.client.LoginCtx(ctx); err != nil {
			return err
		}
		info, err := q.client.GetTransferInfoCtx(ctx)
		if err != nil {
			return err
		}
		if info != nil {
			stats.DownloadSpeed = float64(info.DlInfoSpeed)
			stats.UploadSpeed = float64(info.UpInfoSpeed)
			stats.DownloadSize = float64(info.DlInfoData)
			stats.UploadSize = float64(info.UpInfoData)
		}
		main, err := q.client.SyncMainDataCtx(ctx, 0)
		if err != nil {
			return err
		}
		if main != nil {
			stats.FreeSpace = float64(main.ServerState.FreeSpaceOnDisk)
			for _, torrent := range main.Torrents {
				size := torrent.TotalSize
				if size <= 0 {
					size = torrent.Size
				}
				stats.UsedSpace += float64(size)
			}
		}
		return nil
	})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return TransferStats{}, ctxErr
		}
		if errors.Is(err, errTransferStatsUnavailable) {
			return TransferStats{}, err
		}
		return TransferStats{}, errTransferStatsQuery
	}
	return stats, nil
}

var _ TransferStatReader = (*QBittorrent)(nil)
