package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

const lockDownloadIntentForAcceptanceSQL = `
	SELECT di.id, di.candidate_key, di.label, di.state, COALESCE(di.torrent_hash, ''), di.subscription_id,
		p.movie_priority, p.tv_priority, p.tv_coverage, COALESCE(p.tv_full_coverage, FALSE),
		success.media_source, success.media_id, success.media_type, p.tv_recovered_target
	FROM download_intents AS di
	LEFT JOIN download_intent_plans AS p ON p.intent_id=di.id
	LEFT JOIN download_intent_success_inputs AS success
		ON success.intent_id=di.id AND success.username=di.username
		AND success.version=1 AND NOT success.legacy
	WHERE di.id=$1 AND di.username=$2
	FOR UPDATE OF di`

const persistDownloadIntentAcceptanceSQL = `
	UPDATE download_intents
	SET state='downloading', torrent_hash=$3,
		updated_at=CASE WHEN state='downloading' THEN updated_at ELSE now() END
	WHERE id=$1 AND username=$2
		AND state IN ('submitting', 'downloading')
		AND (
			NULLIF(BTRIM(torrent_hash), '') IS NULL
			OR UPPER(BTRIM(torrent_hash))=$3
		)
	RETURNING id, candidate_key, label, state, COALESCE(torrent_hash, '')`

const lockDownloadIntentSubscriptionForAcceptanceSQL = `
	SELECT media_type, internal_state, current_download_intent_id,
		completed_at IS NOT NULL, season, config, media_source, media_id
	FROM subscriptions
	WHERE id=$1 AND username=$2
	FOR UPDATE`

const lockDownloadIntentEpisodesForAcceptanceSQL = `
	SELECT id, season, episode, state
	FROM subscription_episodes
	WHERE subscription_id=$1 AND download_intent_id=$2
	ORDER BY id
	FOR UPDATE`

const consumeDownloadIntentEpisodesSQL = `
	UPDATE subscription_episodes
	SET state='indexed', updated_at=now()
	WHERE subscription_id=$1 AND download_intent_id=$2
		AND state IN ('acquired', 'downloading')`

const countMissingDownloadIntentSubscriptionEpisodesSQL = `
	SELECT count(*)
	FROM subscription_episodes
	WHERE subscription_id=$1 AND state='missing'`

const countMissingCurrentTargetEpisodesSQL = `
	WITH requested AS (
		SELECT $2::integer AS season, unnest($3::integer[]) AS episode
	)
	SELECT count(*)
	FROM requested
	LEFT JOIN subscription_episodes AS se
		ON se.subscription_id=$1
		AND se.season=requested.season
		AND se.episode=requested.episode
	WHERE se.id IS NULL OR se.state='missing'`

const countMissingCurrentTargetEpisodesAfterCoverageSQL = `
	WITH requested AS (
		SELECT $2::integer AS season, unnest($3::integer[]) AS episode
	)
	SELECT count(*)
	FROM requested
	LEFT JOIN subscription_episodes AS se
		ON se.subscription_id=$1
		AND se.season=requested.season
		AND se.episode=requested.episode
	WHERE (se.id IS NULL OR se.state='missing')
		AND requested.episode <> ALL($4::integer[])`

const completeDownloadIntentSubscriptionSQL = `
	UPDATE subscriptions
	SET internal_state=$4, v2_state=$5, completed_at=now(),
		current_download_intent_id=NULL, config=config || $6::jsonb,
		updated_at=now()
	WHERE id=$1 AND username=$2 AND current_download_intent_id=$3
		AND completed_at IS NULL
		AND internal_state IN ('acquired', 'downloading')`

const continueDownloadIntentSubscriptionSQL = `
	UPDATE subscriptions
	SET internal_state=$4, v2_state=$5,
		current_download_intent_id=NULL, config=config || $6::jsonb,
		updated_at=now()
	WHERE id=$1 AND username=$2 AND current_download_intent_id=$3
		AND completed_at IS NULL
		AND internal_state IN ('acquired', 'downloading')`

const preservePendingIdentificationDownloadIntentSubscriptionSQL = `
	UPDATE subscriptions
	SET internal_state=$4, v2_state=$5,
		current_download_intent_id=NULL, config=config || $6::jsonb,
		updated_at=now()
	WHERE id=$1 AND username=$2 AND current_download_intent_id=$3
		AND completed_at IS NULL AND internal_state=$4`

type downloadIntentAcceptanceTarget struct {
	intent         downloader.Intent
	subscriptionID *int64
	moviePriority  *int
	tvFacts        downloadIntentTVFacts
}

type downloadIntentAcceptanceSubscription struct {
	mediaType     string
	mediaTypeNull bool
	mediaSource   string
	mediaID       string
	state         subscription.State
	currentIntent *uuid.UUID
	completed     bool
	season        *int
	rawConfig     []byte
	config        map[string]any
}

// AcceptDownloadIntent 原子记录下载器确认及该意图的订阅进度。
// 总数补认计划消费冻结的本轮缺集事实；其余计划消费绑定的逐集行。
// 调用本方法前必须完成外部下载器调用。
func (s *Store) AcceptDownloadIntent(
	ctx context.Context,
	username string,
	intentID uuid.UUID,
	torrentHash string,
) (downloader.Intent, bool, error) {
	return s.acceptDownloadIntent(ctx, username, intentID, torrentHash, false)
}

// AcceptDownloadIntentAndFinishSearch 在异步确认下载时一并执行搜索收尾。
// 仅本意图实际结算的普通 TV 中间 N 转为 R；旧意图重放不补改父订阅。
func (s *Store) AcceptDownloadIntentAndFinishSearch(
	ctx context.Context,
	username string,
	intentID uuid.UUID,
	torrentHash string,
) (downloader.Intent, bool, error) {
	return s.acceptDownloadIntent(ctx, username, intentID, torrentHash, true)
}

func (s *Store) acceptDownloadIntent(
	ctx context.Context,
	username string,
	intentID uuid.UUID,
	torrentHash string,
	finishSearch bool,
) (downloader.Intent, bool, error) {
	username, torrentHash, err := normalizeDownloadIntentAcceptance(username, intentID, torrentHash)
	if err != nil {
		return downloader.Intent{}, false, err
	}
	if ctx == nil {
		return downloader.Intent{}, false, ErrDownloadIntentPersistence
	}
	if err := ctx.Err(); err != nil {
		return downloader.Intent{}, false, err
	}
	if !downloadIntentStoreReady(s) {
		return downloader.Intent{}, false, ErrDownloadIntentPersistence
	}

	tx, err := s.db.Pool.Begin(ctx)
	if err != nil {
		return downloader.Intent{}, false, downloadIntentPersistenceError(ctx)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	target, err := lockDownloadIntentForAcceptance(ctx, tx, username, intentID)
	if err != nil {
		return downloader.Intent{}, false, err
	}
	accepted, err := acceptedDownloadIntent(target.intent, torrentHash)
	if err != nil {
		return downloader.Intent{}, false, err
	}
	accepted, err = persistDownloadIntentAcceptance(ctx, tx, username, accepted)
	if err != nil {
		return downloader.Intent{}, false, err
	}
	if target.intent.State == downloader.IntentSubmitting {
		if err := enqueueAcceptedDownloadNotification(ctx, tx, username, intentID); err != nil {
			return downloader.Intent{}, false, err
		}
	}

	subscriptionReleased := target.subscriptionID == nil
	if target.subscriptionID != nil {
		if *target.subscriptionID <= 0 {
			return downloader.Intent{}, false, ErrDownloadIntentPersistence
		}
		parent, err := lockDownloadIntentSubscriptionForAcceptance(
			ctx, tx, username, *target.subscriptionID,
		)
		if err != nil {
			return downloader.Intent{}, false, err
		}
		alreadyReleased := target.intent.State == downloader.IntentDownloading &&
			downloadIntentAcceptanceWasAlreadyReleased(parent, intentID)
		preservePending := downloadIntentAcceptanceCanPreservePending(parent, intentID)
		parentCanSettle := downloadIntentAcceptanceCanSettleParent(parent, intentID) || preservePending
		settleParent := parentCanSettle
		if parent.mediaTypeNull {
			// The public V2 update may explicitly clear the media type while an
			// Add result is still unknown. Persist the irreversible downloader
			// acceptance, but leave parent and bound episode facts for repair.
			settleParent = false
		}
		if settleParent {
			parent.config, err = decodeDownloadIntentAcceptanceConfig(parent.rawConfig)
			if err != nil {
				// qBittorrent acceptance is irreversible. Keep the intent fact
				// even when legacy subscription JSON cannot be safely interpreted;
				// a later repair/reconciliation can retry the parent transition.
				settleParent = false
			}
		}
		if settleParent && (target.tvFacts.invalid || !downloadIntentAcceptanceFactsReadable(parent, target.tvFacts, target.moviePriority)) {
			settleParent = false
		}
		var consumedEpisodes []subscription.Episode
		if target.tvFacts.recovered != nil && !target.tvFacts.invalid {
			consumedEpisodes = append([]subscription.Episode(nil), target.tvFacts.recovered.ExpectedCoverage...)
		} else if !target.tvFacts.invalid && (!parentCanSettle || settleParent) {
			consumedEpisodes, err = consumeDownloadIntentEpisodes(
				ctx, tx, *target.subscriptionID, intentID,
				func(episodes []subscription.Episode) bool {
					if settleParent && isDownloadIntentAcceptanceTV(parent.mediaType) {
						if _, _, err := buildTVQualityAcceptancePatch(parent, episodes, 0, target.tvFacts); err != nil {
							settleParent = false
							return false
						}
					}
					return true
				},
			)
			if err != nil {
				return downloader.Intent{}, false, err
			}
		}
		if settleParent {
			var settleErr error
			if preservePending {
				settleErr = preservePendingIdentificationDownloadIntentAcceptance(
					ctx, tx, username, *target.subscriptionID, intentID, parent, consumedEpisodes, target.moviePriority, target.tvFacts,
				)
			} else {
				settleErr = settleDownloadIntentAcceptanceSubscription(
					ctx, tx, username, *target.subscriptionID, intentID, parent, consumedEpisodes, target.moviePriority, target.tvFacts,
					finishSearch,
				)
			}
			if settleErr != nil {
				return downloader.Intent{}, false, settleErr
			}
			subscriptionReleased = true
		} else if alreadyReleased {
			subscriptionReleased = true
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return downloader.Intent{}, false, downloadIntentPersistenceError(ctx)
	}
	return accepted, subscriptionReleased, nil
}

func downloadIntentAcceptanceWasAlreadyReleased(
	parent downloadIntentAcceptanceSubscription,
	intentID uuid.UUID,
) bool {
	if parent.currentIntent != nil && *parent.currentIntent == intentID {
		return false
	}
	switch parent.state {
	case subscription.PendingIdentification, subscription.PendingSearch, subscription.Active, subscription.Completed:
		return true
	default:
		return false
	}
}

func normalizeDownloadIntentAcceptance(
	username string,
	intentID uuid.UUID,
	torrentHash string,
) (string, string, error) {
	username = strings.TrimSpace(username)
	if username == "" || intentID == uuid.Nil {
		return "", "", ErrInvalidDownloadIntent
	}
	normalizedHash, err := downloader.NormalizeTorrentHash(torrentHash)
	if err != nil {
		return "", "", ErrInvalidDownloadIntent
	}
	return username, normalizedHash, nil
}

func lockDownloadIntentForAcceptance(
	ctx context.Context,
	tx pgx.Tx,
	username string,
	intentID uuid.UUID,
) (downloadIntentAcceptanceTarget, error) {
	var target downloadIntentAcceptanceTarget
	var tvCoverage []byte
	var recoveredJSON []byte
	var mediaSource, mediaID, mediaType sql.NullString
	err := tx.QueryRow(ctx, lockDownloadIntentForAcceptanceSQL, intentID, username).Scan(
		&target.intent.ID,
		&target.intent.CandidateKey,
		&target.intent.Label,
		&target.intent.State,
		&target.intent.TorrentHash,
		&target.subscriptionID,
		&target.moviePriority,
		&target.tvFacts.priority,
		&tvCoverage,
		&target.tvFacts.fullCoverage,
		&mediaSource,
		&mediaID,
		&mediaType,
		&recoveredJSON,
	)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return downloadIntentAcceptanceTarget{}, contextErr
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return downloadIntentAcceptanceTarget{}, ErrDownloadIntentNotFound
		}
		return downloadIntentAcceptanceTarget{}, ErrDownloadIntentPersistence
	}
	if tvCoverage != nil {
		if err := json.Unmarshal(tvCoverage, &target.tvFacts.coverage); err != nil || target.tvFacts.coverage == nil || len(target.tvFacts.coverage) > maxSubscriptionEpisodes {
			target.tvFacts.invalid = true
		}
		for _, episode := range target.tvFacts.coverage {
			if !validSubscriptionEpisode(episode) {
				target.tvFacts.invalid = true
			}
		}
	}
	if recoveredJSON != nil {
		target.tvFacts.recovered, err = decodeDownloadIntentTVRecoveredTarget(recoveredJSON)
		if err != nil {
			target.tvFacts.invalid = true
		}
	}
	target.tvFacts.mediaSource, target.tvFacts.mediaID = mediaSource.String, mediaID.String
	target.tvFacts.mediaType, target.tvFacts.mediaKnown = mediaType.String, mediaType.Valid
	target.intent, err = normalizeDownloadIntent(target.intent)
	if err != nil {
		return downloadIntentAcceptanceTarget{}, ErrDownloadIntentPersistence
	}
	return target, nil
}

func acceptedDownloadIntent(intent downloader.Intent, torrentHash string) (downloader.Intent, error) {
	if intent.State != downloader.IntentSubmitting && intent.State != downloader.IntentDownloading {
		return downloader.Intent{}, ErrDownloadIntentStateConflict
	}
	if intent.TorrentHash != "" {
		storedHash, err := downloader.NormalizeTorrentHash(intent.TorrentHash)
		if err != nil {
			return downloader.Intent{}, ErrDownloadIntentPersistence
		}
		if storedHash != torrentHash {
			return downloader.Intent{}, ErrDownloadIntentStateConflict
		}
	}
	accepted, err := intent.WithTorrentHash(torrentHash)
	if err != nil {
		return downloader.Intent{}, ErrDownloadIntentPersistence
	}
	accepted, err = accepted.Transition(downloader.IntentDownloading)
	if err != nil {
		return downloader.Intent{}, ErrDownloadIntentPersistence
	}
	return accepted, nil
}

func persistDownloadIntentAcceptance(
	ctx context.Context,
	tx pgx.Tx,
	username string,
	intent downloader.Intent,
) (downloader.Intent, error) {
	stored, err := scanDownloadIntent(tx.QueryRow(
		ctx,
		persistDownloadIntentAcceptanceSQL,
		intent.ID,
		username,
		intent.TorrentHash,
	))
	if err == nil {
		return stored, nil
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return downloader.Intent{}, contextErr
	}
	return downloader.Intent{}, ErrDownloadIntentPersistence
}

func lockDownloadIntentSubscriptionForAcceptance(
	ctx context.Context,
	tx pgx.Tx,
	username string,
	subscriptionID int64,
) (downloadIntentAcceptanceSubscription, error) {
	var parent downloadIntentAcceptanceSubscription
	var rawConfig []byte
	var mediaType sql.NullString
	var mediaSource, mediaID sql.NullString
	err := tx.QueryRow(
		ctx,
		lockDownloadIntentSubscriptionForAcceptanceSQL,
		subscriptionID,
		username,
	).Scan(
		&mediaType,
		&parent.state,
		&parent.currentIntent,
		&parent.completed,
		&parent.season,
		&rawConfig,
		&mediaSource,
		&mediaID,
	)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return downloadIntentAcceptanceSubscription{}, contextErr
		}
		return downloadIntentAcceptanceSubscription{}, ErrDownloadIntentPersistence
	}
	parent.mediaType = mediaType.String
	parent.mediaTypeNull = !mediaType.Valid
	parent.mediaSource, parent.mediaID = mediaSource.String, mediaID.String
	if _, err := subscription.V2State(parent.state); err != nil {
		return downloadIntentAcceptanceSubscription{}, ErrDownloadIntentPersistence
	}
	parent.rawConfig = append([]byte(nil), rawConfig...)
	return parent, nil
}

func consumeDownloadIntentEpisodes(
	ctx context.Context,
	tx pgx.Tx,
	subscriptionID int64,
	intentID uuid.UUID,
	guards ...func([]subscription.Episode) bool,
) ([]subscription.Episode, error) {
	rows, err := tx.Query(
		ctx,
		lockDownloadIntentEpisodesForAcceptanceSQL,
		subscriptionID,
		intentID,
	)
	if err != nil {
		return nil, downloadIntentPersistenceError(ctx)
	}
	eligible := int64(0)
	consumed := make([]subscription.Episode, 0)
	for rows.Next() {
		var (
			episodeID int64
			episode   subscription.Episode
			state     subscription.SubscriptionEpisodeState
		)
		if err := rows.Scan(&episodeID, &episode.Season, &episode.Episode, &state); err != nil {
			rows.Close()
			return nil, downloadIntentPersistenceError(ctx)
		}
		if episodeID <= 0 || !validSubscriptionEpisode(episode) || !validSubscriptionEpisodeState(state) {
			rows.Close()
			return nil, ErrDownloadIntentPersistence
		}
		if state == subscription.SubscriptionEpisodeAcquired ||
			state == subscription.SubscriptionEpisodeDownloading {
			eligible++
			consumed = append(consumed, episode)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, downloadIntentPersistenceError(ctx)
	}
	rows.Close()
	for _, guard := range guards {
		if guard != nil && !guard(consumed) {
			return nil, nil
		}
	}

	command, err := tx.Exec(
		ctx,
		consumeDownloadIntentEpisodesSQL,
		subscriptionID,
		intentID,
	)
	if err != nil {
		return nil, downloadIntentPersistenceError(ctx)
	}
	if command.RowsAffected() != eligible {
		return nil, ErrDownloadIntentPersistence
	}
	return consumed, nil
}

func downloadIntentAcceptanceCanSettleParent(
	parent downloadIntentAcceptanceSubscription,
	intentID uuid.UUID,
) bool {
	if parent.completed || parent.currentIntent == nil || *parent.currentIntent != intentID {
		return false
	}
	return parent.state == subscription.Acquired || parent.state == subscription.Downloading
}

func downloadIntentAcceptanceCanPreservePending(
	parent downloadIntentAcceptanceSubscription,
	intentID uuid.UUID,
) bool {
	if parent.completed || parent.currentIntent == nil || *parent.currentIntent != intentID {
		return false
	}
	return parent.state == subscription.PendingIdentification
}

func preservePendingIdentificationDownloadIntentAcceptance(
	ctx context.Context,
	tx pgx.Tx,
	username string,
	subscriptionID int64,
	intentID uuid.UUID,
	parent downloadIntentAcceptanceSubscription,
	consumedEpisodes []subscription.Episode,
	moviePriority *int,
	tvFacts downloadIntentTVFacts,
) error {
	_, progressPatch, err := buildDownloadIntentAcceptanceSettlement(ctx, tx, subscriptionID, parent, consumedEpisodes, moviePriority, tvFacts)
	if err != nil {
		return err
	}
	projected, err := subscription.V2State(subscription.PendingIdentification)
	if err != nil {
		return ErrDownloadIntentPersistence
	}
	command, err := tx.Exec(
		ctx,
		preservePendingIdentificationDownloadIntentSubscriptionSQL,
		subscriptionID,
		username,
		intentID,
		subscription.PendingIdentification,
		projected,
		progressPatch,
	)
	if err != nil {
		return downloadIntentPersistenceError(ctx)
	}
	if command.RowsAffected() != 1 {
		return ErrDownloadIntentPersistence
	}
	return nil
}

func settleDownloadIntentAcceptanceSubscription(
	ctx context.Context,
	tx pgx.Tx,
	username string,
	subscriptionID int64,
	intentID uuid.UUID,
	parent downloadIntentAcceptanceSubscription,
	consumedEpisodes []subscription.Episode,
	moviePriority *int,
	tvFacts downloadIntentTVFacts,
	finishSearch bool,
) error {
	next, progressPatch, err := buildDownloadIntentAcceptanceSettlement(ctx, tx, subscriptionID, parent, consumedEpisodes, moviePriority, tvFacts)
	if err != nil {
		return err
	}
	if finishSearch && isDownloadIntentAcceptanceTV(parent.mediaType) &&
		!downloadIntentAcceptanceBestVersion(parent.config) && next == subscription.PendingSearch {
		next = subscription.Active
	}
	v2State, err := subscription.V2State(next)
	if err != nil {
		return ErrDownloadIntentPersistence
	}
	query := completeDownloadIntentSubscriptionSQL
	if next != subscription.Completed {
		query = continueDownloadIntentSubscriptionSQL
	}
	command, err := tx.Exec(
		ctx,
		query,
		subscriptionID,
		username,
		intentID,
		next,
		v2State,
		progressPatch,
	)
	if err != nil {
		return downloadIntentPersistenceError(ctx)
	}
	if command.RowsAffected() != 1 {
		return ErrDownloadIntentPersistence
	}
	return nil
}

func downloadIntentAcceptanceParentState(mediaType string, missing int64, targetKnown ...bool) (subscription.State, error) {
	if missing < 0 {
		return "", ErrDownloadIntentPersistence
	}
	switch strings.ToLower(strings.TrimSpace(mediaType)) {
	case "电影", "movie":
		return subscription.Completed, nil
	case "电视剧", "tv":
		if len(targetKnown) > 0 && !targetKnown[0] {
			return subscription.PendingSearch, nil
		}
		if missing > 0 {
			return subscription.PendingSearch, nil
		}
		return subscription.Completed, nil
	default:
		return "", ErrDownloadIntentPersistence
	}
}

func downloadIntentAcceptanceCurrentTargetProgress(
	ctx context.Context,
	tx pgx.Tx,
	subscriptionID int64,
	parent downloadIntentAcceptanceSubscription,
	consumed []subscription.Episode,
	acceptedCoverage ...[]subscription.Episode,
) ([]subscription.Episode, int64, bool, error) {
	mediaType := strings.ToLower(strings.TrimSpace(parent.mediaType))
	if mediaType != "电视剧" && mediaType != "tv" {
		var missing int64
		if err := tx.QueryRow(ctx, countMissingDownloadIntentSubscriptionEpisodesSQL, subscriptionID).Scan(&missing); err != nil {
			return nil, 0, false, downloadIntentPersistenceError(ctx)
		}
		return consumed, missing, true, nil
	}
	target, legacy, err := downloadIntentAcceptanceTVTarget(parent.season, parent.config)
	if err != nil {
		return nil, 0, false, err
	}
	if legacy || target.Knowledge != subscription.TVTargetKnown || len(target.TargetEpisodes) == 0 {
		var missing int64
		if legacy {
			if err := tx.QueryRow(ctx, countMissingDownloadIntentSubscriptionEpisodesSQL, subscriptionID).Scan(&missing); err != nil {
				return nil, 0, false, downloadIntentPersistenceError(ctx)
			}
		}
		return nil, missing, false, nil
	}
	numbers := make([]int32, 0, len(target.TargetEpisodes))
	allowed := make(map[subscription.Episode]struct{}, len(target.TargetEpisodes))
	for _, episode := range target.TargetEpisodes {
		numbers = append(numbers, int32(episode.Episode))
		allowed[episode] = struct{}{}
	}
	filtered := make([]subscription.Episode, 0, len(consumed))
	for _, episode := range consumed {
		if _, ok := allowed[episode]; ok {
			filtered = append(filtered, episode)
		}
	}
	var missing int64
	query := countMissingCurrentTargetEpisodesSQL
	args := []any{subscriptionID, target.Snapshot.Season, numbers}
	if len(acceptedCoverage) > 0 {
		acceptedNumbers := make([]int32, 0, len(acceptedCoverage[0]))
		for _, episode := range acceptedCoverage[0] {
			if episode.Episode > 0 && episode.Episode <= maxSubscriptionEpisodeNumber {
				acceptedNumbers = append(acceptedNumbers, int32(episode.Episode))
			}
		}
		query = countMissingCurrentTargetEpisodesAfterCoverageSQL
		args = append(args, acceptedNumbers)
	}
	if err := tx.QueryRow(
		ctx, query, args...,
	).Scan(&missing); err != nil {
		return nil, 0, false, downloadIntentPersistenceError(ctx)
	}
	return filtered, missing, true, nil
}

func downloadIntentAcceptanceTVTarget(
	season *int,
	config map[string]any,
) (subscription.TVTargetResolution, bool, error) {
	seasonNumber := 1
	if season != nil {
		seasonNumber = *season
	}
	snapshot := subscription.TVTargetSnapshot{Season: seasonNumber}
	for key, destination := range map[string]*int{
		"start_episode": &snapshot.StartEpisode,
		"total_episode": &snapshot.TotalEpisode,
		"lack_episode":  &snapshot.LackEpisode,
	} {
		value, exists := config[key]
		if !exists || value == nil {
			continue
		}
		parsed, ok := subscriptionInteger(value)
		if !ok {
			return subscription.TVTargetResolution{}, false, ErrDownloadIntentPersistence
		}
		*destination = parsed
	}
	for key, destination := range map[string]*bool{
		"manual_total_episode": &snapshot.ManualTotalEpisode,
		"best_version":         &snapshot.BestVersion,
		"best_version_full":    &snapshot.BestVersionFull,
	} {
		value, exists := config[key]
		if !exists || value == nil {
			continue
		}
		flag, ok := downloadIntentAcceptanceConfigFlag(value)
		if key == "best_version" || key == "best_version_full" {
			flag, ok = downloadIntentAcceptanceQualityFlag(value)
		}
		if !ok {
			return subscription.TVTargetResolution{}, false, ErrDownloadIntentPersistence
		}
		*destination = flag
	}
	note, err := downloadIntentAcceptanceNote(config["note"])
	if err != nil {
		return subscription.TVTargetResolution{}, false, err
	}
	for episode := range note {
		snapshot.Note = append(snapshot.Note, episode)
	}
	sort.Ints(snapshot.Note)
	if value := config["current_priority"]; value != nil {
		parsed, ok := subscriptionInteger(value)
		if !ok {
			return subscription.TVTargetResolution{}, false, ErrDownloadIntentPersistence
		}
		snapshot.CurrentPriority = &parsed
	}
	snapshot.EpisodePriority, err = subscription.NormalizeEpisodePriorities(config["episode_priority"])
	if err != nil {
		return subscription.TVTargetResolution{}, false, ErrDownloadIntentPersistence
	}
	target, err := subscription.DescribeTVTarget(snapshot)
	if err != nil {
		return subscription.TVTargetResolution{}, false, ErrDownloadIntentPersistence
	}
	_, hasTotal := config["total_episode"]
	return target, !hasTotal, nil
}

func downloadIntentAcceptanceConfigFlag(value any) (bool, bool) {
	if flag, ok := value.(bool); ok {
		return flag, true
	}
	integer, ok := subscriptionInteger(value)
	return integer != 0, ok && (integer == 0 || integer == 1)
}

func downloadIntentAcceptanceQualityFlag(value any) (bool, bool) {
	if flag, ok := value.(bool); ok {
		return flag, true
	}
	integer, ok := subscriptionInteger(value)
	return integer != 0, ok
}
