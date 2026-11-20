package database

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

var (
	// ErrInvalidDownloadIntentPlan 表示执行计划缺少不可变身份或包含不安全的公开资源标识。
	ErrInvalidDownloadIntentPlan = errors.New("下载执行计划无效")
	// ErrDownloadIntentPlanNotFound 对不存在及其他用户的计划统一返回，避免泄露所有权。
	ErrDownloadIntentPlanNotFound = errors.New("下载执行计划不存在")
	// ErrDownloadIntentPlanConflict 表示同一下载意图已绑定不同的不可变执行计划。
	ErrDownloadIntentPlanConflict = errors.New("下载执行计划冲突")
	// ErrDownloadIntentPlanPersistence 表示数据库或加密组件暂时无法安全完成计划持久化。
	ErrDownloadIntentPlanPersistence = errors.New("下载执行计划持久化失败")
)

var (
	downloadIntentPlanResolverKindPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)
	downloadIntentPlanResourceIDPattern   = regexp.MustCompile(`^(?:[A-Za-z0-9][A-Za-z0-9._-]*|sha256:[0-9a-f]{64})$`)
	errStoredDownloadIntentPlanInvalid    = errors.New("持久化下载执行计划数据无效")
)

const (
	maxDownloadIntentPlanResolverKindLength = 64
	maxDownloadIntentPlanResourceIDLength   = 255
	maxDownloadIntentPlanSavePathLength     = 4096
)

// DownloadIntentPlan 是下载意图绑定的不可变执行快照。
// ResourceSecret 只在内存中保存明文，数据库仅持久化 SecretBox 密文。
type DownloadIntentPlan struct {
	IntentID        uuid.UUID
	SiteID          int64
	DownloaderID    string
	ResolverKind    string
	ResolverVersion int16
	ResourceID      string
	ResourceSecret  string
	SavePath        string
	Paused          bool
	CreatedAt       time.Time
	// MoviePriority freezes the filter-assigned movie quality; nil means unknown.
	MoviePriority *int
	// TV facts freeze selected or metadata coverage; nil means legacy unknown.
	// In automatic TV plans, Paused distinguishes explicit unpack selection.
	TVPriority     *int
	TVCoverage     []subscription.Episode
	TVFullCoverage bool
	// TVRecoveredTarget 冻结配置总数仍为零时本轮缺集与预期消费，独立于文件事实。
	TVRecoveredTarget *subscription.TVRecoveredTarget
}

type pgxQueryRower interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

const createDownloadIntentPlanSQL = `
	INSERT INTO download_intent_plans (
		intent_id, site_id, downloader_id, resolver_kind, resolver_version,
		resource_id, resource_secret, save_path, paused, movie_priority,
		tv_priority, tv_coverage, tv_full_coverage, tv_recovered_target
	)
	SELECT $1, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13::jsonb, $14, $15::jsonb
	FROM download_intents AS di
	WHERE di.id=$1 AND di.username=$2
	ON CONFLICT (intent_id) DO NOTHING
	RETURNING intent_id, site_id, downloader_id, resolver_kind, resolver_version,
		resource_id, resource_secret, save_path, paused, created_at, movie_priority,
		tv_priority, tv_coverage, tv_full_coverage, tv_recovered_target`

const getDownloadIntentPlanSQL = `
	SELECT p.intent_id, p.site_id, p.downloader_id, p.resolver_kind, p.resolver_version,
		p.resource_id, p.resource_secret, p.save_path, p.paused, p.created_at, p.movie_priority,
		p.tv_priority, p.tv_coverage, p.tv_full_coverage, p.tv_recovered_target
	FROM download_intent_plans AS p
	JOIN download_intents AS di ON di.id=p.intent_id
	WHERE p.intent_id=$1 AND di.username=$2`

// CreateDownloadIntentPlan 为当前用户的下载意图幂等创建不可变执行计划。
func (s *Store) CreateDownloadIntentPlan(
	ctx context.Context,
	username string,
	plan DownloadIntentPlan,
) (DownloadIntentPlan, error) {
	username, plan, err := normalizeDownloadIntentPlanInput(username, plan)
	if err != nil {
		return DownloadIntentPlan{}, err
	}
	if !downloadIntentPlanStoreReady(s) || ctx == nil {
		return DownloadIntentPlan{}, ErrDownloadIntentPlanPersistence
	}
	return s.createDownloadIntentPlan(ctx, s.db.Pool, username, plan)
}

// CreateDownloadIntentPlanTx 在调用者提供的事务内幂等创建不可变执行计划。
func (s *Store) CreateDownloadIntentPlanTx(
	ctx context.Context,
	tx pgx.Tx,
	username string,
	plan DownloadIntentPlan,
) (DownloadIntentPlan, error) {
	username, plan, err := normalizeDownloadIntentPlanInput(username, plan)
	if err != nil {
		return DownloadIntentPlan{}, err
	}
	if !downloadIntentPlanTxReady(s, tx) || ctx == nil {
		return DownloadIntentPlan{}, ErrDownloadIntentPlanPersistence
	}
	return s.createDownloadIntentPlan(ctx, tx, username, plan)
}

// GetDownloadIntentPlan 按意图 UUID 和规范化用户名读取并解密执行计划。
func (s *Store) GetDownloadIntentPlan(
	ctx context.Context,
	username string,
	intentID uuid.UUID,
) (DownloadIntentPlan, error) {
	username, err := normalizeDownloadIntentPlanOwner(username, intentID)
	if err != nil {
		return DownloadIntentPlan{}, err
	}
	if !downloadIntentPlanStoreReady(s) || ctx == nil {
		return DownloadIntentPlan{}, ErrDownloadIntentPlanPersistence
	}
	return s.getDownloadIntentPlan(ctx, s.db.Pool, username, intentID)
}

// GetDownloadIntentPlanTx 在调用者提供的事务内按 owner 读取并解密执行计划。
func (s *Store) GetDownloadIntentPlanTx(
	ctx context.Context,
	tx pgx.Tx,
	username string,
	intentID uuid.UUID,
) (DownloadIntentPlan, error) {
	username, err := normalizeDownloadIntentPlanOwner(username, intentID)
	if err != nil {
		return DownloadIntentPlan{}, err
	}
	if !downloadIntentPlanTxReady(s, tx) || ctx == nil {
		return DownloadIntentPlan{}, ErrDownloadIntentPlanPersistence
	}
	return s.getDownloadIntentPlan(ctx, tx, username, intentID)
}

func (s *Store) createDownloadIntentPlan(
	ctx context.Context,
	querier pgxQueryRower,
	username string,
	plan DownloadIntentPlan,
) (DownloadIntentPlan, error) {
	encryptedSecret := ""
	if plan.ResourceSecret != "" {
		var err error
		encryptedSecret, err = s.secretBox.Encrypt(plan.ResourceSecret)
		if err != nil {
			return DownloadIntentPlan{}, downloadIntentPlanPersistenceError(ctx)
		}
	}

	stored, err := s.scanDownloadIntentPlan(querier.QueryRow(
		ctx,
		createDownloadIntentPlanSQL,
		plan.IntentID,
		username,
		plan.SiteID,
		plan.DownloaderID,
		plan.ResolverKind,
		plan.ResolverVersion,
		plan.ResourceID,
		encryptedSecret,
		plan.SavePath,
		plan.Paused,
		plan.MoviePriority,
		plan.TVPriority,
		encodeDownloadIntentTVCoverage(plan.TVCoverage),
		plan.TVFullCoverage,
		encodeDownloadIntentTVRecoveredTarget(plan.TVRecoveredTarget),
	))
	if err == nil {
		return stored, nil
	}
	if contextErr := downloadIntentPlanContextError(ctx); contextErr != nil {
		return DownloadIntentPlan{}, contextErr
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return DownloadIntentPlan{}, ErrDownloadIntentPlanPersistence
	}

	existing, err := s.getDownloadIntentPlan(ctx, querier, username, plan.IntentID)
	if err != nil {
		return DownloadIntentPlan{}, err
	}
	if sameDownloadIntentPlan(existing, plan) {
		return existing, nil
	}
	return DownloadIntentPlan{}, ErrDownloadIntentPlanConflict
}

func (s *Store) getDownloadIntentPlan(
	ctx context.Context,
	querier pgxQueryRower,
	username string,
	intentID uuid.UUID,
) (DownloadIntentPlan, error) {
	stored, err := s.scanDownloadIntentPlan(querier.QueryRow(ctx, getDownloadIntentPlanSQL, intentID, username))
	if err == nil {
		return stored, nil
	}
	if contextErr := downloadIntentPlanContextError(ctx); contextErr != nil {
		return DownloadIntentPlan{}, contextErr
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return DownloadIntentPlan{}, ErrDownloadIntentPlanNotFound
	}
	return DownloadIntentPlan{}, ErrDownloadIntentPlanPersistence
}

func (s *Store) scanDownloadIntentPlan(row rowScanner) (DownloadIntentPlan, error) {
	var (
		plan            DownloadIntentPlan
		encryptedSecret string
		tvCoverage      []byte
		recoveredTarget []byte
	)
	if err := row.Scan(
		&plan.IntentID,
		&plan.SiteID,
		&plan.DownloaderID,
		&plan.ResolverKind,
		&plan.ResolverVersion,
		&plan.ResourceID,
		&encryptedSecret,
		&plan.SavePath,
		&plan.Paused,
		&plan.CreatedAt,
		&plan.MoviePriority,
		&plan.TVPriority,
		&tvCoverage,
		&plan.TVFullCoverage,
		&recoveredTarget,
	); err != nil {
		return DownloadIntentPlan{}, err
	}
	if tvCoverage != nil {
		if err := json.Unmarshal(tvCoverage, &plan.TVCoverage); err != nil || plan.TVCoverage == nil {
			return DownloadIntentPlan{}, errStoredDownloadIntentPlanInvalid
		}
	}
	if recoveredTarget != nil {
		if err := json.Unmarshal(recoveredTarget, &plan.TVRecoveredTarget); err != nil || plan.TVRecoveredTarget == nil {
			return DownloadIntentPlan{}, errStoredDownloadIntentPlanInvalid
		}
	}

	if encryptedSecret != "" {
		plainText, err := s.secretBox.Decrypt(encryptedSecret)
		if err != nil {
			return DownloadIntentPlan{}, errStoredDownloadIntentPlanInvalid
		}
		plan.ResourceSecret = plainText
	}
	normalized, err := normalizeDownloadIntentPlanFields(plan)
	if err != nil {
		return DownloadIntentPlan{}, errStoredDownloadIntentPlanInvalid
	}
	normalized.CreatedAt = plan.CreatedAt
	return normalized, nil
}

func normalizeDownloadIntentPlanInput(
	username string,
	plan DownloadIntentPlan,
) (string, DownloadIntentPlan, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return "", DownloadIntentPlan{}, ErrInvalidDownloadIntentPlan
	}
	normalized, err := normalizeDownloadIntentPlanFields(plan)
	if err != nil {
		return "", DownloadIntentPlan{}, err
	}
	normalized.CreatedAt = time.Time{}
	return username, normalized, nil
}

func normalizeDownloadIntentPlanFields(plan DownloadIntentPlan) (DownloadIntentPlan, error) {
	recovered, recoveredErr := subscription.NormalizeTVRecoveredTarget(plan.TVRecoveredTarget)
	if recoveredErr != nil || recovered != nil && (plan.MoviePriority != nil || plan.TVPriority == nil || plan.TVCoverage == nil) {
		return DownloadIntentPlan{}, ErrInvalidDownloadIntentPlan
	}
	plan.TVRecoveredTarget = recovered
	downloaderID, err := uuid.Parse(strings.TrimSpace(plan.DownloaderID))
	if err != nil {
		return DownloadIntentPlan{}, ErrInvalidDownloadIntentPlan
	}
	plan.DownloaderID = downloaderID.String()
	plan.ResolverKind = strings.ToLower(strings.TrimSpace(plan.ResolverKind))
	plan.ResourceID = strings.TrimSpace(plan.ResourceID)
	plan.SavePath = strings.TrimSpace(plan.SavePath)
	if plan.MoviePriority != nil {
		priority := *plan.MoviePriority
		if priority < -2147483648 || priority > 2147483647 {
			return DownloadIntentPlan{}, ErrInvalidDownloadIntentPlan
		}
		plan.MoviePriority = &priority
	}
	if plan.TVPriority != nil {
		priority := *plan.TVPriority
		if priority < -2147483648 || priority > 2147483647 || plan.MoviePriority != nil || plan.TVCoverage == nil {
			return DownloadIntentPlan{}, ErrInvalidDownloadIntentPlan
		}
		plan.TVPriority = &priority
	}
	if plan.MoviePriority != nil && (plan.TVCoverage != nil || plan.TVFullCoverage) ||
		plan.TVFullCoverage && plan.TVCoverage == nil || len(plan.TVCoverage) > maxSubscriptionEpisodes {
		return DownloadIntentPlan{}, ErrInvalidDownloadIntentPlan
	}
	if plan.TVCoverage != nil {
		coverage := slices.Clone(plan.TVCoverage)
		for _, episode := range coverage {
			if !validSubscriptionEpisode(episode) {
				return DownloadIntentPlan{}, ErrInvalidDownloadIntentPlan
			}
		}
		slices.SortFunc(coverage, func(a, b subscription.Episode) int {
			if a.Season != b.Season {
				return a.Season - b.Season
			}
			return a.Episode - b.Episode
		})
		plan.TVCoverage = slices.Compact(coverage)
	}
	if plan.IntentID == uuid.Nil ||
		plan.SiteID <= 0 ||
		plan.ResolverVersion <= 0 ||
		len(plan.ResolverKind) > maxDownloadIntentPlanResolverKindLength ||
		!downloadIntentPlanResolverKindPattern.MatchString(plan.ResolverKind) ||
		len(plan.ResourceID) > maxDownloadIntentPlanResourceIDLength ||
		!downloadIntentPlanResourceIDPattern.MatchString(plan.ResourceID) ||
		len(plan.SavePath) > maxDownloadIntentPlanSavePathLength ||
		strings.IndexByte(plan.SavePath, 0) >= 0 {
		return DownloadIntentPlan{}, ErrInvalidDownloadIntentPlan
	}
	return plan, nil
}

func normalizeDownloadIntentPlanOwner(username string, intentID uuid.UUID) (string, error) {
	username = strings.TrimSpace(username)
	if username == "" || intentID == uuid.Nil {
		return "", ErrInvalidDownloadIntentPlan
	}
	return username, nil
}

func sameDownloadIntentPlan(left, right DownloadIntentPlan) bool {
	return left.IntentID == right.IntentID &&
		left.SiteID == right.SiteID &&
		left.DownloaderID == right.DownloaderID &&
		left.ResolverKind == right.ResolverKind &&
		left.ResolverVersion == right.ResolverVersion &&
		left.ResourceID == right.ResourceID &&
		left.ResourceSecret == right.ResourceSecret &&
		left.SavePath == right.SavePath &&
		left.Paused == right.Paused && sameDownloadIntentMoviePriority(left.MoviePriority, right.MoviePriority) &&
		sameDownloadIntentMoviePriority(left.TVPriority, right.TVPriority) &&
		(left.TVCoverage == nil) == (right.TVCoverage == nil) && slices.Equal(left.TVCoverage, right.TVCoverage) &&
		left.TVFullCoverage == right.TVFullCoverage && reflect.DeepEqual(left.TVRecoveredTarget, right.TVRecoveredTarget)
}

func encodeDownloadIntentTVRecoveredTarget(target *subscription.TVRecoveredTarget) []byte {
	if target == nil {
		return nil
	}
	encoded, _ := json.Marshal(target)
	return encoded
}

func encodeDownloadIntentTVCoverage(coverage []subscription.Episode) []byte {
	if coverage == nil {
		return nil
	}
	encoded, _ := json.Marshal(coverage)
	return encoded
}

func sameDownloadIntentMoviePriority(left, right *int) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}

func downloadIntentPlanStoreReady(store *Store) bool {
	return store != nil && store.db != nil && store.db.Pool != nil && store.secretBox != nil
}

func downloadIntentPlanTxReady(store *Store, tx pgx.Tx) bool {
	return store != nil && store.secretBox != nil && tx != nil
}

func downloadIntentPlanContextError(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}

func downloadIntentPlanPersistenceError(ctx context.Context) error {
	if err := downloadIntentPlanContextError(ctx); err != nil {
		return err
	}
	return ErrDownloadIntentPlanPersistence
}
