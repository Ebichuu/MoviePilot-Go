package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	maxSiteTimingSamples = 10
	siteStatisticLayout  = "2006-01-02 15:04:05"
)

var errSiteStatisticDomainRequired = errors.New("site statistic domain is required")

// SiteStatistic 是兼容 MoviePilot V2 站点统计接口的持久化模型。
type SiteStatistic struct {
	Domain       string         `json:"domain"`
	Success      int            `json:"success"`
	Fail         int            `json:"fail"`
	Seconds      int            `json:"seconds"`
	LastState    int            `json:"lst_state"`
	LastModified string         `json:"lst_mod_date"`
	Note         map[string]int `json:"note"`
}

// RecordSiteTest 原子记录一次站点连通性结果。
// 成功时 elapsed 按完整秒计数，少于一秒按一秒记录；失败不会修改历史耗时。
func (s *Store) RecordSiteTest(ctx context.Context, domain string, success bool, elapsed time.Duration) (returnErr error) {
	domain = normalizeStatisticDomain(domain)
	if domain == "" {
		return fmt.Errorf("记录站点统计: %w", errSiteStatisticDomainRequired)
	}

	tx, err := s.db.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("开始记录站点统计事务: %w", err)
	}
	defer func() {
		rollbackErr := tx.Rollback(ctx)
		if rollbackErr != nil && !errors.Is(rollbackErr, pgx.ErrTxClosed) && returnErr == nil {
			returnErr = fmt.Errorf("回滚站点统计 %q: %w", domain, rollbackErr)
		}
	}()

	// 先用唯一域名完成幂等建行，再显式锁行，避免并发测试丢失计数。
	if _, err := tx.Exec(ctx, `
		INSERT INTO site_statistics (domain)
		VALUES ($1)
		ON CONFLICT (domain) DO NOTHING`, domain); err != nil {
		return fmt.Errorf("初始化站点统计 %q: %w", domain, err)
	}

	current, err := scanSiteStatistic(tx.QueryRow(ctx, `
		SELECT domain, success, fail, seconds, lst_state, lst_mod_date, note
		FROM site_statistics
		WHERE domain=$1
		FOR UPDATE`, domain))
	if err != nil {
		return fmt.Errorf("锁定站点统计 %q: %w", domain, err)
	}

	modified := formatSiteStatisticTime(time.Now(), siteStatisticLocation())
	next := applySiteTestResult(current, success, elapsedSeconds(elapsed), modified)
	note, err := json.Marshal(next.Note)
	if err != nil {
		return fmt.Errorf("编码站点统计 %q 耗时样本: %w", domain, err)
	}
	result, err := tx.Exec(ctx, `
		UPDATE site_statistics
		SET success=$2, fail=$3, seconds=$4, lst_state=$5, lst_mod_date=$6, note=$7::jsonb
		WHERE domain=$1`, next.Domain, next.Success, next.Fail, next.Seconds,
		next.LastState, next.LastModified, note)
	if err != nil {
		return fmt.Errorf("更新站点统计 %q: %w", domain, err)
	}
	if result.RowsAffected() != 1 {
		return fmt.Errorf("更新站点统计 %q: %w", domain, pgx.ErrNoRows)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("提交站点统计 %q: %w", domain, err)
	}
	return nil
}

// ListSiteStatistics 返回全部站点统计，按域名稳定排序。
func (s *Store) ListSiteStatistics(ctx context.Context) ([]SiteStatistic, error) {
	rows, err := s.db.Pool.Query(ctx, `
		SELECT domain, success, fail, seconds, lst_state, lst_mod_date, note
		FROM site_statistics
		ORDER BY domain`)
	if err != nil {
		return nil, fmt.Errorf("查询站点统计: %w", err)
	}
	defer rows.Close()

	statistics := make([]SiteStatistic, 0)
	for rows.Next() {
		statistic, scanErr := scanSiteStatistic(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		statistics = append(statistics, statistic)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历站点统计: %w", err)
	}
	return statistics, nil
}

// SiteStatisticByDomain 按规范化域名查询统计。
// 未找到时返回带查询域名的零计数记录、found=false 和 nil 错误。
func (s *Store) SiteStatisticByDomain(ctx context.Context, domain string) (statistic SiteStatistic, found bool, err error) {
	domain = normalizeStatisticDomain(domain)
	statistic, err = scanSiteStatistic(s.db.Pool.QueryRow(ctx, `
		SELECT domain, success, fail, seconds, lst_state, lst_mod_date, note
		FROM site_statistics
		WHERE domain=$1`, domain))
	if errors.Is(err, pgx.ErrNoRows) {
		return SiteStatistic{Domain: domain}, false, nil
	}
	if err != nil {
		return SiteStatistic{}, false, fmt.Errorf("查询站点统计 %q: %w", domain, err)
	}
	return statistic, true, nil
}

func scanSiteStatistic(row rowScanner) (SiteStatistic, error) {
	var statistic SiteStatistic
	var note []byte
	if err := row.Scan(
		&statistic.Domain,
		&statistic.Success,
		&statistic.Fail,
		&statistic.Seconds,
		&statistic.LastState,
		&statistic.LastModified,
		&note,
	); err != nil {
		return SiteStatistic{}, fmt.Errorf("读取站点统计: %w", err)
	}
	if err := json.Unmarshal(note, &statistic.Note); err != nil {
		return SiteStatistic{}, fmt.Errorf("解析站点统计 %q 耗时样本: %w", statistic.Domain, err)
	}
	if statistic.Note == nil {
		statistic.Note = make(map[string]int)
	}
	return statistic, nil
}

func applySiteTestResult(current SiteStatistic, success bool, elapsed int, modified string) SiteStatistic {
	next := current
	next.LastModified = modified
	if success {
		next.Success++
		next.LastState = 0
		next.Note, next.Seconds = addSiteTimingSample(current.Note, modified, elapsed)
		return next
	}

	next.Fail++
	next.LastState = 1
	next.Note = cloneSiteTimingSamples(current.Note)
	return next
}

func addSiteTimingSample(samples map[string]int, modified string, seconds int) (map[string]int, int) {
	next := cloneSiteTimingSamples(samples)
	if seconds < 1 {
		seconds = 1
	}
	next[nextSiteTimingKey(next, modified)] = seconds

	keys := make([]string, 0, len(next))
	for key := range next {
		keys = append(keys, key)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(keys)))
	if len(keys) > maxSiteTimingSamples {
		for _, key := range keys[maxSiteTimingSamples:] {
			delete(next, key)
		}
	}

	total := 0
	for _, value := range next {
		if value < 1 {
			value = 1
		}
		total += value
	}
	if len(next) == 0 {
		return next, 0
	}
	average := total / len(next)
	if average < 1 {
		average = 1
	}
	return next, average
}

// nextSiteTimingKey 为同一秒内的多个成功样本生成可被 V2 前端解析的唯一键。
// 基础键被裁剪后仍沿用已有的最大毫秒序号，避免重新使用旧时间键。
func nextSiteTimingKey(samples map[string]int, modified string) string {
	maxSequence := -1
	if _, exists := samples[modified]; exists {
		maxSequence = 0
	}

	prefix := modified + "."
	for key := range samples {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		sequence, err := strconv.Atoi(strings.TrimPrefix(key, prefix))
		if err != nil || sequence < 1 {
			continue
		}
		if sequence > maxSequence {
			maxSequence = sequence
		}
	}
	if maxSequence < 0 {
		return modified
	}
	return fmt.Sprintf("%s.%03d", modified, maxSequence+1)
}

func cloneSiteTimingSamples(samples map[string]int) map[string]int {
	cloned := make(map[string]int, len(samples)+1)
	for key, value := range samples {
		cloned[key] = value
	}
	return cloned
}

func elapsedSeconds(elapsed time.Duration) int {
	seconds := int(elapsed / time.Second)
	if seconds < 1 {
		return 1
	}
	return seconds
}

func normalizeStatisticDomain(domain string) string {
	return strings.ToLower(strings.TrimSpace(domain))
}

func formatSiteStatisticTime(value time.Time, location *time.Location) string {
	if location == nil {
		location = time.Local
	}
	return value.In(location).Format(siteStatisticLayout)
}

func siteStatisticLocation() *time.Location {
	name := strings.TrimSpace(os.Getenv("MOVIEPILOT_TIMEZONE"))
	if name == "" {
		name = "Asia/Shanghai"
	}
	location, err := time.LoadLocation(name)
	if err != nil {
		return time.Local
	}
	return location
}
