package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/moviepilot-go/moviepilot-go/internal/auth"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

var (
	// ErrInvalidSubscriptionState 表示前端提交了不支持的订阅控制状态。
	ErrInvalidSubscriptionState = errors.New("无效的订阅状态")
	// ErrSubscriptionNotFound 对不存在、无权访问或已完成的订阅统一返回，避免泄露行存在性。
	ErrSubscriptionNotFound = errors.New("订阅不存在")
	// ErrSubscriptionNotReady 表示订阅尚未完成入库确认，不能进入完成历史。
	ErrSubscriptionNotReady = errors.New("订阅尚未达到可完成状态")
	// ErrInvalidSubscriptionHistoryPage 表示历史分页参数超出受控范围。
	ErrInvalidSubscriptionHistoryPage = errors.New("订阅历史分页参数无效")
)

const (
	maxSubscriptionHistoryPage     = 1_000_000
	maxSubscriptionHistoryPageSize = 100
)

// Store 封装业务模块允许使用的 PostgreSQL 查询。
type Store struct {
	db        *DB
	secretBox *auth.SecretBox
}

// NewSubscription 是创建订阅所需的最小输入，不暴露数据库 JSON 细节给 HTTP 层。
type NewSubscription struct {
	Username    string
	Name        string
	MediaType   string
	Year        string
	Season      *int
	MediaSource string
	MediaID     string
	State       subscription.State
	Config      map[string]any
}

// Subscription 是订阅读取模型，V2 扩展字段保存在 Config 中。
type Subscription struct {
	ID        int64
	Username  string
	Name      string
	MediaType string
	// MediaTypeNull distinguishes an official V2 NULL type from an explicitly
	// empty string while keeping existing in-memory fixtures source compatible.
	MediaTypeNull   bool
	Year            string
	YearNull        bool
	Season          *int
	MediaSource     string
	MediaSourceNull bool
	MediaID         string
	MediaIDNull     bool
	InternalState   subscription.State
	V2State         string
	Config          map[string]any
	CompletedAt     *time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// V2 将内部订阅投影为官方前端使用的字段集合。
func (s Subscription) V2() map[string]any {
	var mediaType any = s.MediaType
	if s.MediaTypeNull {
		mediaType = nil
	}
	var year any = s.Year
	if s.YearNull {
		year = nil
	}
	var mediaSource any = s.MediaSource
	if s.MediaSourceNull {
		mediaSource = nil
	}
	var mediaID any = s.MediaID
	if s.MediaIDNull {
		mediaID = nil
	}
	result := map[string]any{
		"id": s.ID, "username": s.Username, "name": s.Name, "type": s.MediaType,
		"year": year, "season": s.Season, "media_source": mediaSource, "media_id": mediaID,
		"state": s.V2State, "date": s.CreatedAt.UTC().Format(time.RFC3339), "last_update": s.UpdatedAt.UTC().Format(time.RFC3339),
		"total_episode": 0, "lack_episode": 0, "completed_episode": nil,
		"sites": []int{}, "filter_groups": []string{}, "note": []any{},
	}
	result["type"] = mediaType
	for key, value := range s.Config {
		result[key] = value
	}
	return result
}

// V2History 将完成行投影为官方订阅历史接口使用的字段语义。
func (s Subscription) V2History() map[string]any {
	result := s.V2()
	result["state"] = nil
	if s.CompletedAt == nil {
		result["date"] = nil
	} else {
		result["date"] = formatSiteStatisticTime(*s.CompletedAt, siteStatisticLocation())
	}
	if s.MediaType == "电视剧" {
		if result["completed_episode"] == nil {
			result["completed_episode"] = completedSubscriptionEpisode(result)
		}
		result["total_episode"] = 0
		result["lack_episode"] = 0
	}
	return result
}

func completedSubscriptionEpisode(fields map[string]any) any {
	total, ok := subscriptionInteger(fields["total_episode"])
	if !ok || total <= 0 {
		return nil
	}
	start, ok := subscriptionInteger(fields["start_episode"])
	if !ok || start <= 0 {
		start = 1
	}
	bestVersion, _ := downloadIntentAcceptanceConfigFlag(fields["best_version"])
	if !bestVersion {
		lack, _ := subscriptionInteger(fields["lack_episode"])
		return max(total-lack, 0)
	}

	base := min(max(start-1, 0), total)
	bestVersionFull, _ := downloadIntentAcceptanceConfigFlag(fields["best_version_full"])
	currentPriority, hasCurrentPriority := subscriptionInteger(fields["current_priority"])
	if bestVersionFull {
		completedTargets := 0
		if hasCurrentPriority && currentPriority == 100 {
			completedTargets = max(total-start+1, 0)
		}
		return min(base+completedTargets, total)
	}
	priorities, err := subscription.NormalizeEpisodePriorities(fields["episode_priority"])
	if err != nil {
		return nil
	}

	snapshot := subscription.TVTargetSnapshot{
		TotalEpisode: total, StartEpisode: start, BestVersion: true,
		EpisodePriority: priorities,
	}
	if hasCurrentPriority {
		snapshot.CurrentPriority = &currentPriority
	}
	facts, err := subscription.DescribeTVBestVersion(snapshot)
	if err != nil {
		return nil
	}
	completedTargets := max(total-start+1, 0) - len(facts.PendingEpisodes)
	return min(base+completedTargets, total)
}

func subscriptionInteger(value any) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, true
	case int32:
		return int(typed), true
	case int64:
		return int(typed), true
	case float64:
		integer := int(typed)
		return integer, typed == float64(integer)
	case json.Number:
		integer, err := typed.Int64()
		return int(integer), err == nil
	default:
		return 0, false
	}
}

// NewStore 创建持久化仓库。
func NewStore(db *DB, secretBox *auth.SecretBox) *Store {
	return &Store{db: db, secretBox: secretBox}
}

// Pool 暴露底层连接池，供插件等旁路组件使用；业务代码仍应走 Store 方法。
func (s *Store) Pool() *pgxpool.Pool {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Pool
}

// CreateSubscription 持久化订阅，并按已识别或未识别身份幂等返回记录 ID。
func (s *Store) CreateSubscription(ctx context.Context, input NewSubscription) (id int64, returnErr error) {
	input, err := normalizeNewSubscription(input)
	if err != nil {
		return 0, err
	}
	encoded, err := json.Marshal(input.Config)
	if err != nil {
		return 0, fmt.Errorf("编码订阅配置: %w", err)
	}
	tx, err := s.db.Pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("开始创建订阅事务: %w", err)
	}
	defer func() {
		rollbackErr := tx.Rollback(ctx)
		if rollbackErr != nil && !errors.Is(rollbackErr, pgx.ErrTxClosed) && returnErr == nil {
			returnErr = fmt.Errorf("回滚创建订阅事务: %w", rollbackErr)
		}
	}()

	id, err = resolveSubscriptionIdentity(
		func() (int64, error) {
			return insertSubscription(ctx, tx, input, encoded)
		},
		func() (int64, error) {
			return refreshExistingSubscription(ctx, tx, input)
		},
	)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("提交创建订阅事务: %w", err)
	}
	return id, nil
}

func insertSubscription(ctx context.Context, tx pgx.Tx, input NewSubscription, encoded []byte) (int64, error) {
	var id int64
	err := tx.QueryRow(ctx, `
		INSERT INTO subscriptions (username, media_type, title, media_source, media_id, year, season, internal_state, v2_state, config)
		VALUES ($1,$2,$3,NULLIF($4,''),NULLIF($5,''),NULLIF($6,''),$7,$8,$9,$10::jsonb)
		ON CONFLICT DO NOTHING
		RETURNING id`, input.Username, input.MediaType, input.Name, input.MediaSource, input.MediaID, input.Year, input.Season, input.State, v2State(input.State), encoded).Scan(&id)
	if err != nil {
		return 0, err
	}
	// 只有真正新建的订阅才发 subscribeAdded：冲突命中「已有订阅」时不重复通知。
	if err := enqueueSubscribeBusinessNotification(ctx, tx, "subscribeAdded", input.Username, id,
		map[string]any{
			"title_year": subscribeTitleYear(input.Name, input.Year),
			"season_fmt": subscribeSeasonFmt(input.Season),
			"username":   input.Username,
		}, subscribeNotificationRoute(input.MediaType)); err != nil {
		return 0, err
	}
	return id, nil
}

func resolveSubscriptionIdentity(insert, refresh func() (int64, error)) (int64, error) {
	const maxAttempts = 3
	for range maxAttempts {
		id, err := insert()
		if err == nil {
			return id, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return 0, fmt.Errorf("保存订阅: %w", err)
		}

		id, err = refresh()
		if err == nil {
			return id, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return 0, fmt.Errorf("读取已有订阅: %w", err)
		}
		// 冲突行可能刚完成并退出部分唯一索引；下一轮重新 INSERT。
	}
	return 0, errors.New("订阅身份在创建期间持续变化，请重试")
}

func normalizeNewSubscription(input NewSubscription) (NewSubscription, error) {
	input.Username = strings.TrimSpace(input.Username)
	input.Name = strings.TrimSpace(input.Name)
	input.MediaType = strings.TrimSpace(input.MediaType)
	input.Year = strings.TrimSpace(input.Year)
	input.MediaSource = strings.ToLower(strings.TrimSpace(input.MediaSource))
	input.MediaID = strings.TrimSpace(input.MediaID)
	if input.Username == "" || input.Name == "" || input.MediaType == "" {
		return NewSubscription{}, errors.New("订阅缺少用户、名称或类型")
	}
	if (input.MediaSource == "") != (input.MediaID == "") {
		return NewSubscription{}, errors.New("订阅媒体来源和媒体 ID 必须同时提供")
	}
	if input.Season != nil && *input.Season < 0 {
		return NewSubscription{}, errors.New("订阅季号不能为负数")
	}
	switch input.State {
	case subscription.PendingIdentification, subscription.PendingSearch, subscription.Paused:
	default:
		return NewSubscription{}, errors.New("订阅初始状态必须是待识别、待搜索或暂停")
	}
	if input.Config == nil {
		input.Config = map[string]any{}
	}
	return input, nil
}

func refreshExistingSubscription(ctx context.Context, tx pgx.Tx, input NewSubscription) (int64, error) {
	var id int64
	if input.MediaSource != "" {
		err := tx.QueryRow(ctx, `
			UPDATE subscriptions
			SET updated_at=now()
			WHERE username=$1
			  AND media_type=$2
			  AND completed_at IS NULL
			  AND lower(btrim(media_source))=$3
			  AND btrim(media_id)=$4
			  AND COALESCE(season, -1)=COALESCE($5::integer, -1)
			RETURNING id`, input.Username, input.MediaType, input.MediaSource, input.MediaID, input.Season).Scan(&id)
		return id, err
	}
	err := tx.QueryRow(ctx, `
		UPDATE subscriptions
		SET updated_at=now()
		WHERE username=$1
		  AND media_type=$2
		  AND completed_at IS NULL
		  AND media_source IS NULL
		  AND media_id IS NULL
		  AND lower(btrim(title))=lower(btrim($3))
		  AND btrim(COALESCE(year, ''))=btrim($4)
		  AND COALESCE(season, -1)=COALESCE($5::integer, -1)
		RETURNING id`, input.Username, input.MediaType, input.Name, input.Year, input.Season).Scan(&id)
	return id, err
}

// ListSubscriptions 查询当前用户可见订阅。
func (s *Store) ListSubscriptions(ctx context.Context, username string, admin bool) ([]Subscription, error) {
	query, args := buildSubscriptionListQuery(username, admin, false)
	return s.listSubscriptions(ctx, query, args)
}

// ListCompletedSubscriptions 查询当前用户可见的完成历史；完成行不会出现在活动订阅列表。
func (s *Store) ListCompletedSubscriptions(ctx context.Context, username string, admin bool) ([]Subscription, error) {
	query, args := buildSubscriptionListQuery(username, admin, true)
	return s.listSubscriptions(ctx, query, args)
}

// ListCompletedSubscriptionsByType 按媒体类型和 owner 在数据库内分页查询完成历史。
func (s *Store) ListCompletedSubscriptionsByType(
	ctx context.Context,
	username string,
	admin bool,
	mediaType string,
	page int,
	count int,
) ([]Subscription, error) {
	query, args, err := buildCompletedSubscriptionPageQuery(username, admin, mediaType, page, count)
	if err != nil {
		return nil, err
	}
	return s.listSubscriptions(ctx, query, args)
}

func buildCompletedSubscriptionPageQuery(
	username string,
	admin bool,
	mediaType string,
	page int,
	count int,
) (string, []any, error) {
	mediaType = strings.TrimSpace(mediaType)
	username = strings.TrimSpace(username)
	if mediaType == "" || page < 1 || page > maxSubscriptionHistoryPage || count < 1 || count > maxSubscriptionHistoryPageSize || (!admin && username == "") {
		return "", nil, ErrInvalidSubscriptionHistoryPage
	}

	offset := int64(page-1) * int64(count)
	query := `SELECT id, username, title, media_type, year, season, media_source, media_id, internal_state, v2_state, config, completed_at, created_at, updated_at FROM subscriptions WHERE completed_at IS NOT NULL AND media_type=$1`
	args := []any{mediaType}
	if !admin {
		query += ` AND username=$2`
		args = append(args, username)
	}
	query += fmt.Sprintf(` ORDER BY completed_at DESC, id DESC LIMIT $%d OFFSET $%d`, len(args)+1, len(args)+2)
	args = append(args, count, offset)
	return query, args, nil
}

func buildSubscriptionListQuery(username string, admin, completed bool) (string, []any) {
	query := `SELECT id, username, title, media_type, year, season, media_source, media_id, internal_state, v2_state, config, completed_at, created_at, updated_at FROM subscriptions`
	if completed {
		query += ` WHERE completed_at IS NOT NULL`
	} else {
		query += ` WHERE completed_at IS NULL`
	}
	args := make([]any, 0, 1)
	if !admin {
		query += ` AND username=$1`
		args = append(args, username)
	}
	if completed {
		query += ` ORDER BY completed_at DESC, id DESC`
	} else {
		query += ` ORDER BY updated_at DESC, id DESC`
	}
	return query, args
}

func (s *Store) listSubscriptions(ctx context.Context, query string, args []any) ([]Subscription, error) {
	rows, err := s.db.Pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("查询订阅: %w", err)
	}
	defer rows.Close()
	items := make([]Subscription, 0)
	for rows.Next() {
		item, scanErr := scanSubscription(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

func buildSubscriptionStateUpdate(id int64, username string, admin bool, state string) (string, []any, error) {
	next, err := subscription.ParseV2ControlState(state)
	if err != nil {
		return "", nil, fmt.Errorf("%w: %v", ErrInvalidSubscriptionState, err)
	}
	projected, err := subscription.V2State(next)
	if err != nil {
		return "", nil, fmt.Errorf("%w: %v", ErrInvalidSubscriptionState, err)
	}
	query := `UPDATE subscriptions SET internal_state=$1, v2_state=$2, updated_at=now() WHERE id=$3 AND completed_at IS NULL AND internal_state<>$4`
	args := []any{next, projected, id, subscription.Completed}
	if !admin {
		query += ` AND username=$5`
		args = append(args, username)
	}
	return query, args, nil
}

// SetSubscriptionV2State 将前端状态码转换为受控内部状态。
func (s *Store) SetSubscriptionV2State(ctx context.Context, id int64, username string, admin bool, state string) error {
	query, args, err := buildSubscriptionStateUpdate(id, username, admin, state)
	if err != nil {
		return err
	}
	result, err := s.db.Pool.Exec(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("更新订阅状态: %w", err)
	}
	if result.RowsAffected() == 0 {
		return ErrSubscriptionNotFound
	}
	return nil
}

// CompleteSubscription 只允许已确认入库的订阅原子进入完成历史。
// 完成状态与 subscribeComplete 业务通知在同一个事务里提交：
// 否则「订阅完成」这件事可能既没进历史也没发通知。
func (s *Store) CompleteSubscription(ctx context.Context, id int64, username string, admin bool) error {
	tx, err := s.db.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("开始完成订阅事务: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	query, args := buildCompleteSubscriptionUpdate(id, username, admin)
	query += ` RETURNING title, year, season, media_type, username`
	var (
		title     string
		year      *string
		season    *int
		mediaType string
		owner     string
	)
	err = tx.QueryRow(ctx, query, args...).Scan(&title, &year, &season, &mediaType, &owner)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("完成订阅: %w", err)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		stateQuery := `SELECT internal_state, completed_at FROM subscriptions WHERE id=$1`
		stateArgs := []any{id}
		if !admin {
			stateQuery += ` AND username=$2`
			stateArgs = append(stateArgs, username)
		}
		var current subscription.State
		var completedAt *time.Time
		if err := tx.QueryRow(ctx, stateQuery, stateArgs...).Scan(&current, &completedAt); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrSubscriptionNotFound
			}
			return fmt.Errorf("检查订阅完成条件: %w", err)
		}
		if completedAt != nil {
			return ErrSubscriptionNotFound
		}
		return fmt.Errorf("%w: 当前状态 %s", ErrSubscriptionNotReady, current)
	}

	yearText := ""
	if year != nil {
		yearText = *year
	}
	if err := enqueueSubscribeBusinessNotification(ctx, tx, "subscribeComplete", owner, id,
		map[string]any{
			"title_year": subscribeTitleYear(title, yearText),
			"season_fmt": subscribeSeasonFmt(season),
			"username":   owner,
			"msgstr":     "",
		}, subscribeNotificationRoute(mediaType)); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("提交完成订阅事务: %w", err)
	}
	return nil
}

func buildCompleteSubscriptionUpdate(id int64, username string, admin bool) (string, []any) {
	query := `UPDATE subscriptions SET internal_state=$1, v2_state=$2, completed_at=now(), updated_at=now() WHERE id=$3 AND completed_at IS NULL AND internal_state=$4`
	args := []any{subscription.Completed, v2State(subscription.Completed), id, subscription.Indexed}
	if !admin {
		query += ` AND username=$5`
		args = append(args, username)
	}
	return query, args
}

// DeleteSubscription 删除指定用户可见的订阅及其逐集状态。
func (s *Store) DeleteSubscription(ctx context.Context, id int64, username string, admin bool) (bool, error) {
	query, args := buildDeleteSubscriptionQuery(id, username, admin, false)
	result, err := s.db.Pool.Exec(ctx, query, args...)
	if err != nil {
		return false, fmt.Errorf("删除订阅: %w", err)
	}
	return result.RowsAffected() > 0, nil
}

// DeleteCompletedSubscription 删除指定用户可见的完成历史，不会影响活动订阅。
func (s *Store) DeleteCompletedSubscription(ctx context.Context, id int64, username string, admin bool) (bool, error) {
	query, args := buildDeleteSubscriptionQuery(id, username, admin, true)
	result, err := s.db.Pool.Exec(ctx, query, args...)
	if err != nil {
		return false, fmt.Errorf("删除订阅历史: %w", err)
	}
	return result.RowsAffected() > 0, nil
}

func buildDeleteSubscriptionQuery(id int64, username string, admin bool, completed bool) (string, []any) {
	query := `DELETE FROM subscriptions WHERE id=$1`
	if completed {
		query += ` AND completed_at IS NOT NULL`
	} else {
		query += ` AND completed_at IS NULL`
	}
	args := []any{id}
	if !admin {
		query += ` AND username=$2`
		args = append(args, strings.TrimSpace(username))
	}
	return query, args
}

func scanSubscription(row interface{ Scan(...any) error }) (Subscription, error) {
	var item Subscription
	var config []byte
	var mediaType, year, mediaSource, mediaID sql.NullString
	if err := row.Scan(&item.ID, &item.Username, &item.Name, &mediaType, &year, &item.Season, &mediaSource, &mediaID, &item.InternalState, &item.V2State, &config, &item.CompletedAt, &item.CreatedAt, &item.UpdatedAt); err != nil {
		return Subscription{}, err
	}
	item.MediaType = mediaType.String
	item.MediaTypeNull = !mediaType.Valid
	item.Year, item.YearNull = year.String, !year.Valid
	item.MediaSource, item.MediaSourceNull = mediaSource.String, !mediaSource.Valid
	item.MediaID, item.MediaIDNull = mediaID.String, !mediaID.Valid
	if err := json.Unmarshal(config, &item.Config); err != nil {
		return Subscription{}, fmt.Errorf("解析订阅配置: %w", err)
	}
	return item, nil
}

func v2State(state subscription.State) string {
	value, _ := subscription.V2State(state)
	return value
}

type rowScanner interface {
	Scan(dest ...any) error
}
