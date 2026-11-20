package database

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/organization"
)

var (
	// ErrInvalidOrganizationSuccessSnapshot reports malformed grouped facts.
	ErrInvalidOrganizationSuccessSnapshot = errors.New("整理成功通知快照无效")
	// ErrOrganizationSuccessSnapshotNotFound hides missing and other-owner batches.
	ErrOrganizationSuccessSnapshotNotFound = errors.New("整理成功通知快照不存在")
	// ErrOrganizationSuccessSnapshotConflict reports an immutable replay mismatch.
	ErrOrganizationSuccessSnapshotConflict = errors.New("整理成功通知快照冲突")
	// ErrOrganizationSuccessSnapshotPersistence is the sanitized storage boundary.
	ErrOrganizationSuccessSnapshotPersistence = errors.New("整理成功通知快照持久化失败")
)

var errStoredOrganizationSuccessSnapshotInvalid = errors.New("持久化整理成功通知快照无效")

const createOrganizationSuccessSnapshotsSQL = `
	INSERT INTO organization_success_snapshots (
		batch_id, group_key, intent_id, version, legacy, username,
		need_notify, manual, background, media_source, media_id,
		title_year, media_type, category, vote_average, poster_url,
		backdrop_url, file_facts
	)
	SELECT $1, candidate.group_key, $2, candidate.version, candidate.legacy,
		candidate.username, candidate.need_notify, candidate.manual,
		candidate.background, candidate.media_source, candidate.media_id,
		candidate.title_year, candidate.media_type, candidate.category,
		candidate.vote_average, candidate.poster_url, candidate.backdrop_url,
		candidate.file_facts::jsonb
	FROM unnest(
		$3::text[], $4::smallint[], $5::boolean[], $6::text[],
		$7::boolean[], $8::boolean[], $9::boolean[], $10::text[],
		$11::text[], $12::text[], $13::text[], $14::text[],
		$15::double precision[], $16::text[], $17::text[], $18::text[]
	) AS candidate(
		group_key, version, legacy, username, need_notify, manual,
		background, media_source, media_id, title_year, media_type,
		category, vote_average, poster_url, backdrop_url, file_facts
	)
	ON CONFLICT (batch_id, group_key) DO NOTHING`

const listOrganizationSuccessSnapshotsSQL = `
	SELECT s.batch_id, s.group_key, s.intent_id, s.version, s.legacy,
		s.username, s.need_notify, s.manual, s.background, s.media_source,
		s.media_id, s.title_year, s.media_type, s.category, s.vote_average,
		s.poster_url, s.backdrop_url, s.file_facts
	FROM organization_success_snapshots AS s
	JOIN organization_batches AS b ON b.id=s.batch_id AND b.intent_id=s.intent_id
	JOIN download_intents AS di ON di.id=s.intent_id
	WHERE s.batch_id=$1 AND di.username=$2
	ORDER BY s.group_key`

const getOrganizationSuccessSnapshotBatchSQL = `
	SELECT b.intent_id, count(f.id)
	FROM organization_batches AS b
	JOIN download_intents AS di ON di.id=b.intent_id
	LEFT JOIN organization_files AS f ON f.batch_id=b.id
	WHERE b.id=$1 AND di.username=$2
	GROUP BY b.intent_id`

// CreateOrganizationSuccessSnapshotsTx creates all grouped snapshots atomically.
func (s *Store) CreateOrganizationSuccessSnapshotsTx(
	ctx context.Context,
	tx pgx.Tx,
	username string,
	snapshots []organization.SuccessSnapshot,
) ([]organization.SuccessSnapshot, error) {
	username, snapshots, encodedFiles, err := normalizeOrganizationSuccessSnapshots(username, snapshots)
	if err != nil {
		return nil, err
	}
	if s == nil || tx == nil || ctx == nil {
		return nil, ErrOrganizationSuccessSnapshotPersistence
	}

	batchID := snapshots[0].BatchID
	intentID := snapshots[0].IntentID
	var storedIntentID uuid.UUID
	var storedFileCount int
	if err := tx.QueryRow(ctx, getOrganizationSuccessSnapshotBatchSQL, batchID, username).Scan(
		&storedIntentID, &storedFileCount,
	); err != nil {
		if contextErr := organizationSuccessSnapshotContextError(ctx); contextErr != nil {
			return nil, contextErr
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrOrganizationSuccessSnapshotNotFound
		}
		return nil, ErrOrganizationSuccessSnapshotPersistence
	}
	if storedIntentID != intentID || storedFileCount != organizationSuccessSnapshotFileCount(snapshots) {
		return nil, ErrOrganizationSuccessSnapshotConflict
	}

	input, err := getDownloadIntentSuccessInput(ctx, tx, username, intentID)
	if err != nil {
		if errors.Is(err, ErrDownloadIntentSuccessInputNotFound) {
			return nil, ErrOrganizationSuccessSnapshotNotFound
		}
		return nil, organizationSuccessSnapshotPersistenceError(ctx)
	}
	if !organizationSuccessSnapshotsMatchInput(snapshots, input) {
		return nil, ErrOrganizationSuccessSnapshotConflict
	}

	arguments := organizationSuccessSnapshotInsertArguments(snapshots, encodedFiles)
	command, err := tx.Exec(ctx, createOrganizationSuccessSnapshotsSQL, arguments...)
	if err != nil || command.RowsAffected() < 0 {
		return nil, organizationSuccessSnapshotPersistenceError(ctx)
	}
	stored, err := listOrganizationSuccessSnapshots(ctx, tx, username, batchID)
	if err != nil {
		return nil, err
	}
	if !sameOrganizationSuccessSnapshots(stored, snapshots) {
		return nil, ErrOrganizationSuccessSnapshotConflict
	}
	return cloneOrganizationSnapshots(stored), nil
}

// ListOrganizationSuccessSnapshots returns all immutable groups for one batch.
func (s *Store) ListOrganizationSuccessSnapshots(
	ctx context.Context,
	username string,
	batchID uuid.UUID,
) ([]organization.SuccessSnapshot, error) {
	username = strings.TrimSpace(username)
	if username == "" || batchID == uuid.Nil {
		return nil, ErrInvalidOrganizationSuccessSnapshot
	}
	if s == nil || s.db == nil || s.db.Pool == nil || ctx == nil {
		return nil, ErrOrganizationSuccessSnapshotPersistence
	}
	return listOrganizationSuccessSnapshots(ctx, s.db.Pool, username, batchID)
}

func listOrganizationSuccessSnapshots(
	ctx context.Context,
	querier interface {
		Query(context.Context, string, ...any) (pgx.Rows, error)
	},
	username string,
	batchID uuid.UUID,
) ([]organization.SuccessSnapshot, error) {
	rows, err := querier.Query(ctx, listOrganizationSuccessSnapshotsSQL, batchID, username)
	if err != nil {
		return nil, organizationSuccessSnapshotPersistenceError(ctx)
	}
	defer rows.Close()
	items := make([]organization.SuccessSnapshot, 0)
	for rows.Next() {
		item, scanErr := scanOrganizationSuccessSnapshot(rows)
		if scanErr != nil {
			return nil, ErrOrganizationSuccessSnapshotPersistence
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, organizationSuccessSnapshotPersistenceError(ctx)
	}
	if len(items) == 0 {
		return nil, ErrOrganizationSuccessSnapshotNotFound
	}
	return items, nil
}

func scanOrganizationSuccessSnapshot(row rowScanner) (organization.SuccessSnapshot, error) {
	var snapshot organization.SuccessSnapshot
	var files []byte
	if err := row.Scan(
		&snapshot.BatchID,
		&snapshot.GroupKey,
		&snapshot.IntentID,
		&snapshot.Version,
		&snapshot.Legacy,
		&snapshot.Username,
		&snapshot.NeedNotify,
		&snapshot.Manual,
		&snapshot.Background,
		&snapshot.Media.Source,
		&snapshot.Media.MediaID,
		&snapshot.Media.TitleYear,
		&snapshot.Media.MediaType,
		&snapshot.Media.Category,
		&snapshot.Media.VoteAverage,
		&snapshot.Media.PosterURL,
		&snapshot.Media.BackdropURL,
		&files,
	); err != nil {
		return organization.SuccessSnapshot{}, err
	}
	if err := json.Unmarshal(files, &snapshot.Files); err != nil {
		return organization.SuccessSnapshot{}, errStoredOrganizationSuccessSnapshotInvalid
	}
	normalized, err := organization.NormalizeSuccessSnapshot(snapshot)
	if err != nil || !sameOrganizationSuccessSnapshot(normalized, snapshot) {
		return organization.SuccessSnapshot{}, errStoredOrganizationSuccessSnapshotInvalid
	}
	return normalized, nil
}

func normalizeOrganizationSuccessSnapshots(
	username string,
	snapshots []organization.SuccessSnapshot,
) (string, []organization.SuccessSnapshot, []string, error) {
	username = strings.TrimSpace(username)
	if username == "" || len(snapshots) == 0 || len(snapshots) > 10_000 {
		return "", nil, nil, ErrInvalidOrganizationSuccessSnapshot
	}
	normalized := cloneOrganizationSnapshots(snapshots)
	batchID := normalized[0].BatchID
	intentID := normalized[0].IntentID
	seenGroups := make(map[string]struct{}, len(normalized))
	seenOrdinals := make(map[int]struct{})
	for index := range normalized {
		item, err := organization.NormalizeSuccessSnapshot(normalized[index])
		if err != nil || item.Username != username || item.BatchID != batchID || item.IntentID != intentID {
			return "", nil, nil, ErrInvalidOrganizationSuccessSnapshot
		}
		if _, exists := seenGroups[item.GroupKey]; exists {
			return "", nil, nil, ErrInvalidOrganizationSuccessSnapshot
		}
		seenGroups[item.GroupKey] = struct{}{}
		for _, file := range item.Files {
			if _, exists := seenOrdinals[file.Ordinal]; exists {
				return "", nil, nil, ErrInvalidOrganizationSuccessSnapshot
			}
			seenOrdinals[file.Ordinal] = struct{}{}
		}
		normalized[index] = item
	}
	if len(seenOrdinals) == 0 {
		return "", nil, nil, ErrInvalidOrganizationSuccessSnapshot
	}
	for ordinal := range len(seenOrdinals) {
		if _, exists := seenOrdinals[ordinal]; !exists {
			return "", nil, nil, ErrInvalidOrganizationSuccessSnapshot
		}
	}
	sort.Slice(normalized, func(i, j int) bool { return normalized[i].GroupKey < normalized[j].GroupKey })
	encoded := make([]string, len(normalized))
	for index := range normalized {
		value, err := json.Marshal(normalized[index].Files)
		if err != nil {
			return "", nil, nil, ErrInvalidOrganizationSuccessSnapshot
		}
		encoded[index] = string(value)
	}
	return username, normalized, encoded, nil
}

func organizationSuccessSnapshotInsertArguments(
	snapshots []organization.SuccessSnapshot,
	encodedFiles []string,
) []any {
	groupKeys := make([]string, len(snapshots))
	versions := make([]int16, len(snapshots))
	legacies := make([]bool, len(snapshots))
	usernames := make([]string, len(snapshots))
	needNotify := make([]bool, len(snapshots))
	manual := make([]bool, len(snapshots))
	background := make([]bool, len(snapshots))
	mediaSources := make([]string, len(snapshots))
	mediaIDs := make([]string, len(snapshots))
	titles := make([]string, len(snapshots))
	mediaTypes := make([]string, len(snapshots))
	categories := make([]string, len(snapshots))
	votes := make([]float64, len(snapshots))
	posters := make([]string, len(snapshots))
	backdrops := make([]string, len(snapshots))
	for index, snapshot := range snapshots {
		groupKeys[index] = snapshot.GroupKey
		versions[index] = int16(snapshot.Version)
		legacies[index] = snapshot.Legacy
		usernames[index] = snapshot.Username
		needNotify[index] = snapshot.NeedNotify
		manual[index] = snapshot.Manual
		background[index] = snapshot.Background
		mediaSources[index] = snapshot.Media.Source
		mediaIDs[index] = snapshot.Media.MediaID
		titles[index] = snapshot.Media.TitleYear
		mediaTypes[index] = snapshot.Media.MediaType
		categories[index] = snapshot.Media.Category
		votes[index] = snapshot.Media.VoteAverage
		posters[index] = snapshot.Media.PosterURL
		backdrops[index] = snapshot.Media.BackdropURL
	}
	return []any{
		snapshots[0].BatchID, snapshots[0].IntentID, groupKeys, versions,
		legacies, usernames, needNotify, manual, background, mediaSources,
		mediaIDs, titles, mediaTypes, categories, votes, posters, backdrops,
		encodedFiles,
	}
}

func organizationSuccessSnapshotsMatchInput(
	snapshots []organization.SuccessSnapshot,
	input organization.SuccessInput,
) bool {
	for _, snapshot := range snapshots {
		if snapshot.IntentID != input.IntentID || snapshot.Version != input.Version || snapshot.Legacy != input.Legacy {
			return false
		}
		if input.Legacy {
			continue
		}
		if snapshot.Username != input.Username || snapshot.Manual != input.Manual ||
			snapshot.Background != input.Background || snapshot.Media != input.Media {
			return false
		}
		for _, file := range snapshot.Files {
			if file.ResourceTerm != input.ResourceTerm {
				return false
			}
		}
	}
	return true
}

func organizationSuccessSnapshotFileCount(snapshots []organization.SuccessSnapshot) int {
	total := 0
	for _, snapshot := range snapshots {
		total += len(snapshot.Files)
	}
	return total
}

func sameOrganizationSuccessSnapshots(left, right []organization.SuccessSnapshot) bool {
	return slices.EqualFunc(left, right, sameOrganizationSuccessSnapshot)
}

func sameOrganizationSuccessSnapshot(left, right organization.SuccessSnapshot) bool {
	return left.BatchID == right.BatchID && left.IntentID == right.IntentID &&
		left.GroupKey == right.GroupKey && left.Version == right.Version &&
		left.Legacy == right.Legacy && left.Username == right.Username &&
		left.NeedNotify == right.NeedNotify && left.Manual == right.Manual &&
		left.Background == right.Background && left.Media == right.Media &&
		slices.EqualFunc(left.Files, right.Files, func(a, b organization.FileFact) bool {
			return a.Ordinal == b.Ordinal && a.Role == b.Role && a.SizeBytes == b.SizeBytes &&
				a.Season == b.Season && a.ResourceTerm == b.ResourceTerm && slices.Equal(a.Episodes, b.Episodes)
		})
}

func cloneOrganizationSnapshots(items []organization.SuccessSnapshot) []organization.SuccessSnapshot {
	cloned := make([]organization.SuccessSnapshot, len(items))
	for index := range items {
		cloned[index] = items[index].Clone()
	}
	return cloned
}

func organizationSuccessSnapshotContextError(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}

func organizationSuccessSnapshotPersistenceError(ctx context.Context) error {
	if err := organizationSuccessSnapshotContextError(ctx); err != nil {
		return err
	}
	return ErrOrganizationSuccessSnapshotPersistence
}
