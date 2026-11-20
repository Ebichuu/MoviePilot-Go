package database

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

var (
	ErrInvalidSubscriptionPublicMutation     = errors.New("订阅公共更新参数无效")
	ErrSubscriptionPublicMutationConflict    = errors.New("订阅公共更新快照冲突")
	ErrSubscriptionPublicMutationPersistence = errors.New("订阅公共更新持久化失败")
)

var subscriptionPublicConfigKeys = map[string]struct{}{
	"keyword": {}, "tmdbid": {}, "doubanid": {}, "bangumiid": {}, "anilistid": {}, "mediaid": {},
	"filter": {}, "include": {}, "exclude": {}, "quality": {}, "resolution": {}, "effect": {},
	"total_episode": {}, "start_episode": {}, "sites": {}, "downloader": {}, "best_version": {},
	"best_version_full": {}, "save_path": {}, "search_imdbid": {}, "custom_words": {},
	"media_category": {}, "filter_groups": {}, "episode_group": {},
}

var subscriptionResetConfigKeys = map[string]struct{}{
	"note": {}, "lack_episode": {}, "current_priority": {}, "episode_priority": {}, "manual_total_episode": {},
}

const getAccessibleActiveSubscriptionSQL = `
	SELECT ` + preciseSearchSubscriptionColumns + `
	FROM subscriptions
	WHERE id=$1 AND completed_at IS NULL AND ($3 OR username=$2)`

const applySubscriptionPublicMutationSQL = `
	UPDATE subscriptions
	SET title=CASE WHEN $13 AND NULLIF($4,'') IS NOT NULL THEN $4 ELSE title END,
		media_type=CASE WHEN $13 THEN $5 ELSE media_type END,
		year=CASE WHEN $13 THEN $6 ELSE year END,
		season=CASE WHEN $13 THEN $7 ELSE season END,
		media_source=CASE WHEN $13 THEN $8 ELSE media_source END,
		media_id=CASE WHEN $13 THEN $9 ELSE media_id END,
		config=config || $10::jsonb,
		internal_state=CASE
			WHEN $11 AND current_download_intent_id IS NULL THEN $12
			ELSE internal_state
		END,
		v2_state=CASE WHEN $11 THEN 'R' ELSE v2_state END,
		updated_at=clock_timestamp()
	WHERE id=$1 AND username=$2 AND completed_at IS NULL AND updated_at=$3
		AND jsonb_typeof(config)='object'
	RETURNING updated_at`

// SubscriptionPublicMutation is one complete V2 public replacement. Nil
// pointer values are persisted as SQL NULL; Config contains only public JSON
// fields, while Target owns the protected TV progress facts.
type SubscriptionPublicMutation struct {
	Username          string
	SubscriptionID    int64
	ExpectedUpdatedAt time.Time
	Name              *string
	MediaType         *string
	Year              *string
	Season            *int
	MediaSource       *string
	MediaID           *string
	Config            map[string]any
	Target            *subscription.TVTargetResolution
	Progress          *SubscriptionPublicProgress
	Reset             bool
	ReplacePublic     bool
}

type SubscriptionPublicProgress struct {
	ManualTotalEpisode bool
	LackEpisode        int
}

func ResolveSubscriptionPublicProgress(observed Subscription, totalEpisode int) (SubscriptionPublicProgress, error) {
	oldTotal := subscriptionConfigIntegerOrZero(observed.Config["total_episode"])
	oldLack := subscriptionConfigIntegerOrZero(observed.Config["lack_episode"])
	manual := false
	if value, exists := observed.Config["manual_total_episode"]; exists && value != nil {
		parsed, ok := downloadIntentAcceptanceConfigFlag(value)
		if !ok {
			return SubscriptionPublicProgress{}, ErrInvalidSubscriptionPublicMutation
		}
		manual = parsed
	}
	resolution, err := subscription.ApplyTVPublicTargetUpdate(
		subscription.TVTargetSnapshot{TotalEpisode: oldTotal, LackEpisode: oldLack, ManualTotalEpisode: manual},
		subscription.TVPublicTargetUpdate{TotalEpisode: &totalEpisode},
	)
	if err != nil {
		return SubscriptionPublicProgress{}, ErrInvalidSubscriptionPublicMutation
	}
	return SubscriptionPublicProgress{
		ManualTotalEpisode: resolution.Snapshot.ManualTotalEpisode,
		LackEpisode:        resolution.Snapshot.LackEpisode,
	}, nil
}

// ResolveSubscriptionTVPublicTarget applies a complete HTTP replacement to
// the protected target facts observed with the same CAS version.
func ResolveSubscriptionTVPublicTarget(
	observed Subscription,
	season int,
	startEpisode int,
	totalEpisode int,
) (subscription.TVTargetResolution, error) {
	snapshot, err := subscriptionTVTargetSnapshot(observed)
	if err != nil {
		return subscription.TVTargetResolution{}, err
	}
	return subscription.ApplyTVPublicTargetUpdate(snapshot, subscription.TVPublicTargetUpdate{
		Season: &season, StartEpisode: &startEpisode, TotalEpisode: &totalEpisode,
	})
}

func subscriptionTVTargetSnapshot(observed Subscription) (subscription.TVTargetSnapshot, error) {
	season := 1
	if observed.Season != nil {
		season = *observed.Season
	}
	snapshot := subscription.TVTargetSnapshot{Season: season}
	for key, destination := range map[string]*int{
		"start_episode": &snapshot.StartEpisode,
		"total_episode": &snapshot.TotalEpisode,
		"lack_episode":  &snapshot.LackEpisode,
	} {
		value, exists := observed.Config[key]
		if !exists || value == nil {
			continue
		}
		parsed, ok := subscriptionInteger(value)
		if !ok {
			return subscription.TVTargetSnapshot{}, ErrInvalidSubscriptionPublicMutation
		}
		*destination = parsed
	}
	for key, destination := range map[string]*bool{
		"manual_total_episode": &snapshot.ManualTotalEpisode,
		"best_version":         &snapshot.BestVersion,
		"best_version_full":    &snapshot.BestVersionFull,
	} {
		value, exists := observed.Config[key]
		if !exists || value == nil {
			continue
		}
		flag, ok := downloadIntentAcceptanceConfigFlag(value)
		if !ok {
			return subscription.TVTargetSnapshot{}, ErrInvalidSubscriptionPublicMutation
		}
		*destination = flag
	}
	note, err := downloadIntentAcceptanceNote(observed.Config["note"])
	if err != nil {
		return subscription.TVTargetSnapshot{}, ErrInvalidSubscriptionPublicMutation
	}
	for episode := range note {
		snapshot.Note = append(snapshot.Note, episode)
	}
	if value, exists := observed.Config["current_priority"]; exists && value != nil {
		priority, ok := subscriptionInteger(value)
		if !ok {
			return subscription.TVTargetSnapshot{}, ErrInvalidSubscriptionPublicMutation
		}
		snapshot.CurrentPriority = &priority
	}
	snapshot.EpisodePriority, err = strictSubscriptionEpisodePriorities(observed.Config["episode_priority"])
	if err != nil {
		return subscription.TVTargetSnapshot{}, ErrInvalidSubscriptionPublicMutation
	}
	return snapshot, nil
}

func strictSubscriptionEpisodePriorities(value any) (map[int]int, error) {
	result, err := subscription.NormalizeEpisodePriorities(value)
	if err != nil {
		return nil, ErrInvalidSubscriptionPublicMutation
	}
	return result, nil
}

// GetAccessibleActiveSubscription reads one owner-visible active row. Admin
// callers receive the real owner, which must be passed into the mutation CAS.
func (s *Store) GetAccessibleActiveSubscription(
	ctx context.Context,
	username string,
	id int64,
	admin bool,
) (Subscription, error) {
	username = strings.TrimSpace(username)
	if ctx == nil || id <= 0 || (!admin && username == "") || !subscriptionEpisodeStoreReady(s) {
		return Subscription{}, ErrSubscriptionNotFound
	}
	item, err := scanSubscription(s.db.Pool.QueryRow(ctx, getAccessibleActiveSubscriptionSQL, id, username, admin))
	if err == nil {
		return item, nil
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return Subscription{}, contextErr
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return Subscription{}, ErrSubscriptionNotFound
	}
	return Subscription{}, ErrSubscriptionPublicMutationPersistence
}

// ApplySubscriptionPublicMutation writes public columns/config and optional TV
// episode membership in one transaction. Existing intent-bound episode facts
// and the current intent are never rewritten.
func (s *Store) ApplySubscriptionPublicMutation(
	ctx context.Context,
	input SubscriptionPublicMutation,
) (time.Time, error) {
	input.Username = strings.TrimSpace(input.Username)
	if ctx == nil || input.Username == "" || input.SubscriptionID <= 0 || input.ExpectedUpdatedAt.IsZero() || input.Config == nil ||
		!validSubscriptionPublicMutationConfig(input) || !subscriptionEpisodeStoreReady(s) {
		return time.Time{}, ErrInvalidSubscriptionPublicMutation
	}
	config := make(map[string]any, len(input.Config)+2)
	for key, value := range input.Config {
		config[key] = value
	}
	if input.Progress != nil {
		if input.Progress.LackEpisode < 0 || input.Progress.LackEpisode > 10000 {
			return time.Time{}, ErrInvalidSubscriptionPublicMutation
		}
		config["manual_total_episode"] = input.Progress.ManualTotalEpisode
		config["lack_episode"] = input.Progress.LackEpisode
	}
	encoded, err := json.Marshal(config)
	if err != nil {
		return time.Time{}, ErrInvalidSubscriptionPublicMutation
	}
	tx, err := s.db.Pool.Begin(ctx)
	if err != nil {
		return time.Time{}, subscriptionPublicMutationPersistenceError(ctx)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	version := input.ExpectedUpdatedAt.UTC()
	if input.Target != nil {
		version, err = reconcileSubscriptionTargetTx(ctx, tx, SubscriptionTargetReconcile{
			Username: input.Username, SubscriptionID: input.SubscriptionID,
			ExpectedUpdatedAt: version, Target: *input.Target,
		})
		if err != nil {
			return time.Time{}, mapSubscriptionPublicTargetError(err)
		}
	}
	var next time.Time
	err = tx.QueryRow(ctx, applySubscriptionPublicMutationSQL,
		input.SubscriptionID, input.Username, version,
		input.Name, input.MediaType, input.Year, input.Season,
		input.MediaSource, input.MediaID, encoded, input.Reset, subscription.Active,
		input.ReplacePublic,
	).Scan(&next)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, classifySubscriptionPublicMutationMiss(ctx, tx, input.SubscriptionID, input.Username)
	}
	if err != nil {
		return time.Time{}, subscriptionPublicMutationPersistenceError(ctx)
	}
	if err := tx.Commit(ctx); err != nil {
		return time.Time{}, subscriptionPublicMutationPersistenceError(ctx)
	}
	return next.UTC(), nil
}

func validSubscriptionPublicMutationConfig(input SubscriptionPublicMutation) bool {
	allowed := subscriptionPublicConfigKeys
	if input.Reset {
		if input.ReplacePublic {
			return false
		}
		allowed = subscriptionResetConfigKeys
	}
	for key := range input.Config {
		if _, ok := allowed[key]; !ok {
			return false
		}
	}
	return true
}

// ResetActiveSubscription applies the V2 reset facts without discarding an
// unresolved current intent or any intent-bound episode history.
func (s *Store) ResetActiveSubscription(ctx context.Context, observed Subscription) (time.Time, error) {
	if observed.ID <= 0 || strings.TrimSpace(observed.Username) == "" || observed.UpdatedAt.IsZero() {
		return time.Time{}, ErrInvalidSubscriptionPublicMutation
	}
	patch := map[string]any{
		"note":                 []int{},
		"lack_episode":         subscriptionConfigIntegerOrZero(observed.Config["total_episode"]),
		"current_priority":     nil,
		"episode_priority":     map[string]int{},
		"manual_total_episode": 0,
	}
	mutation := SubscriptionPublicMutation{
		Username: observed.Username, SubscriptionID: observed.ID,
		ExpectedUpdatedAt: observed.UpdatedAt, Config: patch, Reset: true,
	}
	if !observed.MediaTypeNull && isTVSubscriptionMediaType(observed.MediaType) {
		target, err := resetSubscriptionTVTarget(observed)
		if err != nil {
			return time.Time{}, err
		}
		mutation.Target = &target
	}
	return s.ApplySubscriptionPublicMutation(ctx, mutation)
}

func resetSubscriptionTVTarget(observed Subscription) (subscription.TVTargetResolution, error) {
	snapshot, err := subscriptionTVTargetSnapshot(observed)
	if err != nil {
		return subscription.TVTargetResolution{}, err
	}
	return subscription.ResetTVTarget(snapshot)
}

func subscriptionConfigIntegerOrZero(value any) int {
	parsed, _ := subscriptionInteger(value)
	return parsed
}

func isTVSubscriptionMediaType(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "tv", "电视剧":
		return true
	default:
		return false
	}
}

func classifySubscriptionPublicMutationMiss(ctx context.Context, tx pgx.Tx, id int64, username string) error {
	var current time.Time
	err := tx.QueryRow(ctx, getActiveSubscriptionTargetVersionSQL, id, username).Scan(&current)
	if err == nil {
		return ErrSubscriptionPublicMutationConflict
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrSubscriptionNotFound
	}
	return ErrSubscriptionPublicMutationPersistence
}

func mapSubscriptionPublicTargetError(err error) error {
	switch {
	case errors.Is(err, ErrSubscriptionTargetConflict):
		return ErrSubscriptionPublicMutationConflict
	case errors.Is(err, ErrSubscriptionTargetNotFound):
		return ErrSubscriptionNotFound
	case errors.Is(err, ErrInvalidSubscriptionTarget):
		return ErrInvalidSubscriptionPublicMutation
	default:
		return err
	}
}

func subscriptionPublicMutationPersistenceError(ctx context.Context) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return ErrSubscriptionPublicMutationPersistence
}
