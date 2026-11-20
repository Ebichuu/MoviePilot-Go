package database

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
)

var (
	// ErrInvalidDownloadIntent 表示待持久化意图缺少安全身份或规范化 Hash。
	ErrInvalidDownloadIntent = errors.New("下载意图无效")
	// ErrInvalidDownloadIntentState 表示 CAS 不符合下载领域状态机。
	ErrInvalidDownloadIntentState = errors.New("下载意图状态无效")
	// ErrDownloadIntentNotFound 对不存在及其他用户的记录统一返回，避免泄露所有权。
	ErrDownloadIntentNotFound = errors.New("下载意图不存在")
	// ErrDownloadIntentStateConflict 表示当前用户的意图存在，但 CAS 预期状态已经过期。
	ErrDownloadIntentStateConflict = errors.New("下载意图状态冲突")
	// ErrDownloadIntentPersistence 表示数据库暂时无法安全完成意图持久化。
	ErrDownloadIntentPersistence = errors.New("下载意图持久化失败")
)

var errStoredDownloadIntentInvalid = errors.New("持久化下载意图数据无效")

const createDownloadIntentSQL = `
	INSERT INTO download_intents (id, username, candidate_key, torrent_hash, label, state, payload)
	VALUES ($1, $2, $3, NULLIF($4::text, ''), $5, $6, '{}'::jsonb)
	ON CONFLICT (username, candidate_key) DO UPDATE
	SET updated_at=now()
	RETURNING id, candidate_key, label, state, COALESCE(torrent_hash, '')`

const getDownloadIntentSQL = `
	SELECT id, candidate_key, label, state, COALESCE(torrent_hash, '')
	FROM download_intents
	WHERE id=$1 AND username=$2`

const compareAndSwapDownloadIntentSQL = `
	UPDATE download_intents
	SET state=$4,
		torrent_hash=COALESCE(NULLIF(torrent_hash, ''), NULLIF($5::text, '')),
		updated_at=now()
	WHERE id=$1 AND username=$2 AND state=ANY($3::text[])
		AND (
			NULLIF($5::text, '') IS NULL
			OR NULLIF(torrent_hash, '') IS NULL
			OR torrent_hash=$5
		)
	RETURNING id, candidate_key, label, state, COALESCE(torrent_hash, '')`

const getDownloadIntentStateSQL = `
	SELECT state FROM download_intents WHERE id=$1 AND username=$2`

// CreateDownloadIntent 按用户和安全候选关联键幂等创建下载意图。
// 冲突重放只刷新更新时间并返回已有行，不覆盖其状态、Hash、标签或 ID。
func (s *Store) CreateDownloadIntent(ctx context.Context, username string, intent downloader.Intent) (downloader.Intent, error) {
	query, args, err := buildCreateDownloadIntent(username, intent)
	if err != nil {
		return downloader.Intent{}, err
	}
	if !downloadIntentStoreReady(s) || ctx == nil {
		return downloader.Intent{}, ErrDownloadIntentPersistence
	}

	return createDownloadIntent(ctx, s.db.Pool, query, args)
}

// CreateDownloadIntentTx 在调用者提供的事务内幂等创建下载意图。
func (s *Store) CreateDownloadIntentTx(
	ctx context.Context,
	tx pgx.Tx,
	username string,
	intent downloader.Intent,
) (downloader.Intent, error) {
	query, args, err := buildCreateDownloadIntent(username, intent)
	if err != nil {
		return downloader.Intent{}, err
	}
	if s == nil || tx == nil || ctx == nil {
		return downloader.Intent{}, ErrDownloadIntentPersistence
	}
	stored, err := createDownloadIntentOnce(ctx, tx, query, args)
	if err != nil {
		return downloader.Intent{}, downloadIntentPersistenceError(ctx)
	}
	return stored, nil
}

// downloadIntentCreateAttempts 限制并发 upsert 的可重试次数。
const downloadIntentCreateAttempts = 5

// createDownloadIntent 在连接池上幂等创建下载意图。同一 (username, candidate_key)
// 并发插入会让 PostgreSQL 给出 40001/40P01，这类错误重试后即可拿到幂等结果，
// 因此这里做有界重试；事务路径不能重试，语句已随事务失效。
func createDownloadIntent(
	ctx context.Context,
	querier pgxQueryRower,
	query string,
	args []any,
) (downloader.Intent, error) {
	for attempt := 1; ; attempt++ {
		stored, err := createDownloadIntentOnce(ctx, querier, query, args)
		if err == nil {
			return stored, nil
		}
		if attempt >= downloadIntentCreateAttempts || !downloadIntentCreateRetryable(err) {
			return downloader.Intent{}, downloadIntentPersistenceError(ctx)
		}
		select {
		case <-ctx.Done():
			return downloader.Intent{}, ctx.Err()
		case <-time.After(time.Duration(attempt) * 5 * time.Millisecond):
		}
	}
}

func createDownloadIntentOnce(
	ctx context.Context,
	querier pgxQueryRower,
	query string,
	args []any,
) (downloader.Intent, error) {
	stored, err := scanDownloadIntent(querier.QueryRow(ctx, query, args...))
	if err != nil {
		return downloader.Intent{}, err
	}
	return stored, nil
}

func downloadIntentCreateRetryable(err error) bool {
	var postgresError *pgconn.PgError
	if !errors.As(err, &postgresError) {
		return false
	}
	return postgresError.Code == "40001" || postgresError.Code == "40P01"
}

// GetDownloadIntent 按 UUID 和规范化用户名读取安全下载意图；不存在及其他用户统一隐藏。
func (s *Store) GetDownloadIntent(ctx context.Context, username string, id uuid.UUID) (downloader.Intent, error) {
	query, args, err := buildGetDownloadIntent(username, id)
	if err != nil {
		return downloader.Intent{}, err
	}
	if ctx == nil {
		return downloader.Intent{}, ErrDownloadIntentPersistence
	}
	if err := ctx.Err(); err != nil {
		return downloader.Intent{}, err
	}
	if !downloadIntentStoreReady(s) {
		return downloader.Intent{}, ErrDownloadIntentPersistence
	}

	return getDownloadIntent(ctx, s.db.Pool, query, args)
}

// GetDownloadIntentTx 在调用者提供的事务内按 UUID 和 owner 读取下载意图。
func (s *Store) GetDownloadIntentTx(
	ctx context.Context,
	tx pgx.Tx,
	username string,
	id uuid.UUID,
) (downloader.Intent, error) {
	query, args, err := buildGetDownloadIntent(username, id)
	if err != nil {
		return downloader.Intent{}, err
	}
	if s == nil || tx == nil || ctx == nil {
		return downloader.Intent{}, ErrDownloadIntentPersistence
	}
	return getDownloadIntent(ctx, tx, query, args)
}

func getDownloadIntent(
	ctx context.Context,
	querier pgxQueryRower,
	query string,
	args []any,
) (downloader.Intent, error) {
	stored, err := scanDownloadIntent(querier.QueryRow(ctx, query, args...))
	if err == nil {
		return stored, nil
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return downloader.Intent{}, contextErr
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return downloader.Intent{}, ErrDownloadIntentNotFound
	}
	return downloader.Intent{}, downloadIntentPersistenceError(ctx)
}

// CompareAndSwapDownloadIntent 在任一 fromStates 状态仍成立时原子写入 next 状态和可选 Hash。
// Hash 为空时保留既有值；非空 Hash 只允许首次写入或相同值幂等重放。
// 状态转换及 Hash 格式由 downloader 领域规则校验。
func (s *Store) CompareAndSwapDownloadIntent(
	ctx context.Context,
	username string,
	id uuid.UUID,
	fromStates []downloader.IntentState,
	next downloader.IntentState,
	torrentHash string,
) (downloader.Intent, error) {
	query, args, err := buildCompareAndSwapDownloadIntent(username, id, fromStates, next, torrentHash)
	if err != nil {
		return downloader.Intent{}, err
	}
	if !downloadIntentStoreReady(s) || ctx == nil {
		return downloader.Intent{}, ErrDownloadIntentPersistence
	}
	return compareAndSwapDownloadIntent(ctx, s.db.Pool, query, args)
}

// CompareAndSwapDownloadIntentTx 在调用者提供的事务内执行 owner-scoped 多来源状态 CAS。
func (s *Store) CompareAndSwapDownloadIntentTx(
	ctx context.Context,
	tx pgx.Tx,
	username string,
	id uuid.UUID,
	fromStates []downloader.IntentState,
	next downloader.IntentState,
	torrentHash string,
) (downloader.Intent, error) {
	query, args, err := buildCompareAndSwapDownloadIntent(username, id, fromStates, next, torrentHash)
	if err != nil {
		return downloader.Intent{}, err
	}
	if s == nil || tx == nil || ctx == nil {
		return downloader.Intent{}, ErrDownloadIntentPersistence
	}
	return compareAndSwapDownloadIntent(ctx, tx, query, args)
}

func compareAndSwapDownloadIntent(
	ctx context.Context,
	querier pgxQueryRower,
	query string,
	args []any,
) (downloader.Intent, error) {
	stored, err := scanDownloadIntent(querier.QueryRow(ctx, query, args...))
	if err == nil {
		return stored, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return downloader.Intent{}, downloadIntentPersistenceError(ctx)
	}

	var current downloader.IntentState
	if err := querier.QueryRow(ctx, getDownloadIntentStateSQL, args[0], args[1]).Scan(&current); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return downloader.Intent{}, ErrDownloadIntentNotFound
		}
		return downloader.Intent{}, downloadIntentPersistenceError(ctx)
	}
	switch current {
	case downloader.IntentPlanned,
		downloader.IntentSubmitting,
		downloader.IntentDownloading,
		downloader.IntentCompleted,
		downloader.IntentFailed:
	default:
		return downloader.Intent{}, ErrDownloadIntentPersistence
	}
	return downloader.Intent{}, ErrDownloadIntentStateConflict
}

func buildCreateDownloadIntent(username string, intent downloader.Intent) (string, []any, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return "", nil, ErrInvalidDownloadIntent
	}

	normalized, err := normalizeDownloadIntent(intent)
	if err != nil {
		return "", nil, err
	}
	args := []any{
		normalized.ID,
		username,
		normalized.CandidateKey,
		normalized.TorrentHash,
		normalized.Label,
		normalized.State,
	}
	return createDownloadIntentSQL, args, nil
}

func buildGetDownloadIntent(username string, id uuid.UUID) (string, []any, error) {
	username = strings.TrimSpace(username)
	if username == "" || id == uuid.Nil {
		return "", nil, ErrInvalidDownloadIntent
	}
	return getDownloadIntentSQL, []any{id, username}, nil
}

func buildCompareAndSwapDownloadIntent(
	username string,
	id uuid.UUID,
	fromStates []downloader.IntentState,
	next downloader.IntentState,
	torrentHash string,
) (string, []any, error) {
	username = strings.TrimSpace(username)
	if username == "" || id == uuid.Nil || len(fromStates) == 0 {
		return "", nil, ErrInvalidDownloadIntent
	}

	normalizedStates := make([]string, 0, len(fromStates))
	seenStates := make(map[downloader.IntentState]struct{}, len(fromStates))
	normalizedHash := ""
	for _, expected := range fromStates {
		if _, exists := seenStates[expected]; exists {
			continue
		}
		seenStates[expected] = struct{}{}

		probe := downloader.Intent{State: expected}
		if strings.TrimSpace(torrentHash) != "" {
			var err error
			probe, err = probe.WithTorrentHash(torrentHash)
			if err != nil {
				return "", nil, ErrInvalidDownloadIntent
			}
		}
		transitioned, err := probe.Transition(next)
		if err != nil {
			return "", nil, ErrInvalidDownloadIntentState
		}
		normalizedHash = transitioned.TorrentHash
		normalizedStates = append(normalizedStates, string(expected))
	}
	if len(normalizedStates) == 0 {
		return "", nil, ErrInvalidDownloadIntent
	}

	args := []any{id, username, normalizedStates, next, normalizedHash}
	return compareAndSwapDownloadIntentSQL, args, nil
}

func normalizeDownloadIntent(intent downloader.Intent) (downloader.Intent, error) {
	if intent.ID == uuid.Nil ||
		!validDownloadIntentOpaqueHex(intent.CandidateKey, "sha256:", 64) ||
		!validDownloadIntentOpaqueHex(intent.Label, "mp-", 32) {
		return downloader.Intent{}, ErrInvalidDownloadIntent
	}

	torrentHash := strings.TrimSpace(intent.TorrentHash)
	intent.TorrentHash = ""
	if torrentHash != "" {
		var err error
		intent, err = intent.WithTorrentHash(torrentHash)
		if err != nil {
			return downloader.Intent{}, ErrInvalidDownloadIntent
		}
	}
	normalized, err := intent.Transition(intent.State)
	if err != nil {
		return downloader.Intent{}, ErrInvalidDownloadIntentState
	}
	return normalized, nil
}

func scanDownloadIntent(row rowScanner) (downloader.Intent, error) {
	var intent downloader.Intent
	if err := row.Scan(
		&intent.ID,
		&intent.CandidateKey,
		&intent.Label,
		&intent.State,
		&intent.TorrentHash,
	); err != nil {
		return downloader.Intent{}, err
	}
	normalized, err := normalizeDownloadIntent(intent)
	if err != nil {
		return downloader.Intent{}, errStoredDownloadIntentInvalid
	}
	return normalized, nil
}

func validDownloadIntentOpaqueHex(value, prefix string, hexLength int) bool {
	if !strings.HasPrefix(value, prefix) || len(value) != len(prefix)+hexLength {
		return false
	}
	digest := strings.TrimPrefix(value, prefix)
	if digest != strings.ToLower(digest) {
		return false
	}
	_, err := hex.DecodeString(digest)
	return err == nil
}

func downloadIntentStoreReady(store *Store) bool {
	return store != nil && store.db != nil && store.db.Pool != nil
}

func downloadIntentPersistenceError(ctx context.Context) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return ErrDownloadIntentPersistence
}
