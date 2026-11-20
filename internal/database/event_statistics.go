package database

import (
	"context"
	"errors"
	"strings"
	"time"
)

// ErrInvalidEventStatistic 表示事件统计入参非法。
var ErrInvalidEventStatistic = errors.New("事件统计参数无效")

const recordEventStatisticSQL = `
	INSERT INTO event_statistics (day, event_type, username, count)
	VALUES ($1::date, $2, $3, 1)
	ON CONFLICT (day, event_type, username)
	DO UPDATE SET count = event_statistics.count + 1, updated_at = now()`

const listEventStatisticsSQL = `
	SELECT day, event_type, sum(count)::bigint AS total
	FROM event_statistics
	WHERE day >= $1::date
	GROUP BY day, event_type
	ORDER BY day DESC, event_type ASC`

// EventStatistic 是按天聚合的一条事件计数。
type EventStatistic struct {
	Day       time.Time
	EventType string
	Count     int64
}

// RecordEventStatistic 把一条事件累加到当天计数。重复调用即重复计数，
// 同一个事件键的幂等由调用方（业务层）保证。
func (s *Store) RecordEventStatistic(
	ctx context.Context,
	day time.Time,
	eventType string,
	username string,
) error {
	if ctx == nil || day.IsZero() {
		return ErrInvalidEventStatistic
	}
	eventType = strings.TrimSpace(eventType)
	if eventType == "" || len(eventType) > 64 {
		return ErrInvalidEventStatistic
	}
	username = strings.TrimSpace(username)
	if len(username) > 128 {
		return ErrInvalidEventStatistic
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if s == nil || s.db == nil || s.db.Pool == nil {
		return ErrInvalidEventStatistic
	}
	if _, err := s.db.Pool.Exec(ctx, recordEventStatisticSQL, day.UTC().Format("2006-01-02"), eventType, username); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ErrInvalidEventStatistic
	}
	return nil
}

// ListEventStatistics 返回 since（含当天）之后按天、按类型聚合的计数。
func (s *Store) ListEventStatistics(ctx context.Context, since time.Time) ([]EventStatistic, error) {
	if ctx == nil || since.IsZero() {
		return nil, ErrInvalidEventStatistic
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s == nil || s.db == nil || s.db.Pool == nil {
		return nil, ErrInvalidEventStatistic
	}
	rows, err := s.db.Pool.Query(ctx, listEventStatisticsSQL, since.UTC().Format("2006-01-02"))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrInvalidEventStatistic
	}
	defer rows.Close()
	stats := make([]EventStatistic, 0, 32)
	for rows.Next() {
		var item EventStatistic
		if err := rows.Scan(&item.Day, &item.EventType, &item.Count); err != nil {
			return nil, ErrInvalidEventStatistic
		}
		item.Day = item.Day.UTC()
		stats = append(stats, item)
	}
	if err := rows.Err(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrInvalidEventStatistic
	}
	return stats, nil
}
