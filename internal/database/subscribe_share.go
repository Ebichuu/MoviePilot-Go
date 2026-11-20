package database

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// ErrSubscribeShareNotFound 表示分享不存在或已被删除。
var ErrSubscribeShareNotFound = errors.New("订阅分享不存在")

// SubscribeShare 是一条订阅分享记录。Sites 为站点 ID 选择，Filter 为过滤规则。
type SubscribeShare struct {
	ID          int64
	ShareKey    string
	Username    string
	Title       string
	MediaType   string
	Year        string
	Season      *int
	MediaSource string
	MediaID     string
	Sites       []int
	Filter      string
	Description string
	Poster      string
	FollowCount int64
	CreatedAt   time.Time
}

const subscribeShareColumns = `id, share_key, username, title, media_type, "year", season,
	media_source, media_id, sites, "filter", description, poster, follow_count, created_at`

func (s *Store) subscribeShareReady() bool {
	return s != nil && s.db != nil && s.db.Pool != nil
}

type subscribeShareScanner interface {
	Scan(...any) error
}

func scanSubscribeShare(row subscribeShareScanner) (SubscribeShare, error) {
	var (
		share  SubscribeShare
		season *int32
		sites  []byte
	)
	if err := row.Scan(&share.ID, &share.ShareKey, &share.Username, &share.Title, &share.MediaType,
		&share.Year, &season, &share.MediaSource, &share.MediaID, &sites, &share.Filter,
		&share.Description, &share.Poster, &share.FollowCount, &share.CreatedAt); err != nil {
		return SubscribeShare{}, err
	}
	if season != nil {
		value := int(*season)
		share.Season = &value
	}
	share.Sites = []int{}
	if len(sites) > 0 {
		_ = json.Unmarshal(sites, &share.Sites)
	}
	return share, nil
}

// CreateSubscribeShare 新建一条订阅分享。
func (s *Store) CreateSubscribeShare(ctx context.Context, share SubscribeShare) (SubscribeShare, error) {
	if !s.subscribeShareReady() {
		return SubscribeShare{}, ErrSubscribeShareNotFound
	}
	if share.Sites == nil {
		share.Sites = []int{}
	}
	encoded, err := json.Marshal(share.Sites)
	if err != nil {
		return SubscribeShare{}, err
	}
	row := s.db.Pool.QueryRow(ctx, `
		INSERT INTO subscribe_shares (
			share_key, username, title, media_type, "year", season,
			media_source, media_id, sites, "filter", description, poster
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		RETURNING `+subscribeShareColumns,
		share.ShareKey, share.Username, share.Title, share.MediaType, share.Year, share.Season,
		share.MediaSource, share.MediaID, encoded, share.Filter, share.Description, share.Poster)
	created, err := scanSubscribeShare(row)
	if err != nil {
		return SubscribeShare{}, err
	}
	return created, nil
}

// ListSubscribeShares 返回某个用户创建的分享。
func (s *Store) ListSubscribeShares(ctx context.Context, username string) ([]SubscribeShare, error) {
	shares := []SubscribeShare{}
	if !s.subscribeShareReady() {
		return shares, nil
	}
	rows, err := s.db.Pool.Query(ctx,
		`SELECT `+subscribeShareColumns+` FROM subscribe_shares WHERE username = $1 ORDER BY id DESC`, username)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		share, scanErr := scanSubscribeShare(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		shares = append(shares, share)
	}
	return shares, rows.Err()
}

// GetSubscribeShare 按主键读取分享。
func (s *Store) GetSubscribeShare(ctx context.Context, id int64) (SubscribeShare, error) {
	if !s.subscribeShareReady() {
		return SubscribeShare{}, ErrSubscribeShareNotFound
	}
	row := s.db.Pool.QueryRow(ctx, `SELECT `+subscribeShareColumns+` FROM subscribe_shares WHERE id = $1`, id)
	share, err := scanSubscribeShare(row)
	if err != nil {
		return SubscribeShare{}, ErrSubscribeShareNotFound
	}
	return share, nil
}

// GetSubscribeShareByKey 按 share_key 读取分享。
func (s *Store) GetSubscribeShareByKey(ctx context.Context, key string) (SubscribeShare, error) {
	if !s.subscribeShareReady() {
		return SubscribeShare{}, ErrSubscribeShareNotFound
	}
	row := s.db.Pool.QueryRow(ctx, `SELECT `+subscribeShareColumns+` FROM subscribe_shares WHERE share_key = $1`, key)
	share, err := scanSubscribeShare(row)
	if err != nil {
		return SubscribeShare{}, ErrSubscribeShareNotFound
	}
	return share, nil
}

// DeleteSubscribeShare 删除自己创建的分享。
func (s *Store) DeleteSubscribeShare(ctx context.Context, username string, id int64) (bool, error) {
	if !s.subscribeShareReady() {
		return false, nil
	}
	tag, err := s.db.Pool.Exec(ctx,
		`DELETE FROM subscribe_shares WHERE id = $1 AND username = $2`, id, username)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// FollowSubscribeShare 关注一条分享；重复关注保持幂等。
func (s *Store) FollowSubscribeShare(ctx context.Context, username string, id int64) (bool, error) {
	if !s.subscribeShareReady() {
		return false, ErrSubscribeShareNotFound
	}
	if _, err := s.GetSubscribeShare(ctx, id); err != nil {
		return false, err
	}
	tag, err := s.db.Pool.Exec(ctx, `
		INSERT INTO subscribe_follows (username, share_id) VALUES ($1, $2)
		ON CONFLICT (username, share_id) DO NOTHING`, username, id)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() > 0 {
		if _, err := s.db.Pool.Exec(ctx,
			`UPDATE subscribe_shares SET follow_count = follow_count + 1 WHERE id = $1`, id); err != nil {
			return false, err
		}
	}
	return true, nil
}

// UnfollowSubscribeShare 取消关注。
func (s *Store) UnfollowSubscribeShare(ctx context.Context, username string, id int64) (bool, error) {
	if !s.subscribeShareReady() {
		return false, nil
	}
	tag, err := s.db.Pool.Exec(ctx,
		`DELETE FROM subscribe_follows WHERE username = $1 AND share_id = $2`, username, id)
	if err != nil {
		return false, err
	}
	removed := tag.RowsAffected() > 0
	if removed {
		if _, err := s.db.Pool.Exec(ctx,
			`UPDATE subscribe_shares SET follow_count = GREATEST(follow_count - 1, 0) WHERE id = $1`, id); err != nil {
			return false, err
		}
	}
	return removed, nil
}

// ListSubscribeFollows 返回某个用户关注的分享。
func (s *Store) ListSubscribeFollows(ctx context.Context, username string) ([]SubscribeShare, error) {
	shares := []SubscribeShare{}
	if !s.subscribeShareReady() {
		return shares, nil
	}
	rows, err := s.db.Pool.Query(ctx, `
		SELECT s.id, s.share_key, s.username, s.title, s.media_type, s."year", s.season,
			s.media_source, s.media_id, s.sites, s."filter", s.description, s.poster,
			s.follow_count, s.created_at
		FROM subscribe_shares s
		JOIN subscribe_follows f ON f.share_id = s.id
		WHERE f.username = $1
		ORDER BY f.created_at DESC`, username)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		share, scanErr := scanSubscribeShare(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		shares = append(shares, share)
	}
	return shares, rows.Err()
}
