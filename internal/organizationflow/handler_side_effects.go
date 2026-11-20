package organizationflow

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

func (h *Handler) enqueueMediaIndexBestEffort(ctx context.Context, requestID uuid.UUID) {
	enqueueCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), mediaIndexEnqueueTimeout)
	defer cancel()
	// The request is already durable and uniquely keyed. A failed notification
	// is deliberately left to the periodic PostgreSQL recovery sweep.
	_ = h.mediaIndexEnqueuer.EnqueueMediaIndex(enqueueCtx, MediaIndexArgs{RequestID: requestID})
}
func (h *Handler) enqueueScrapeBestEffort(ctx context.Context, ids []uuid.UUID) {
	if h.scrapeEnqueuer == nil {
		return
	}
	enqueueCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), mediaIndexEnqueueTimeout)
	defer cancel()
	for _, id := range ids {
		if id == uuid.Nil {
			continue
		}
		_ = h.scrapeEnqueuer.EnqueueScrape(enqueueCtx, id)
	}
}
func (h *Handler) enqueueNotificationBestEffort(ctx context.Context, ids []uuid.UUID) {
	if h.notificationEnqueuer == nil {
		return
	}
	enqueueCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), mediaIndexEnqueueTimeout)
	defer cancel()
	for _, id := range ids {
		if id == uuid.Nil {
			continue
		}
		_ = h.notificationEnqueuer.EnqueueNotification(enqueueCtx, id)
	}
}
func (h *Handler) sendOrganizationNotification(ctx context.Context, batch OrganizationBatch) {
	if h.notificationSender == nil {
		return
	}

	// Use a detached context with bounded timeout to avoid blocking the worker.
	notifCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()

	fileCount := len(batch.Plan.Operations)
	title := "整理完成"
	text := "文件已成功整理到媒体库"
	if fileCount > 0 {
		text = fmt.Sprintf("已整理 %d 个文件到媒体库", fileCount)
	}

	msg := NotificationMessage{
		Type:        "整理入库",
		ContentType: "organizeSuccess",
		Title:       title,
		Text:        text,
	}

	_ = h.notificationSender.Send(notifCtx, msg)
}
