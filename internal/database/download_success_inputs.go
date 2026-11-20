package database

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/organization"
)

var (
	// ErrInvalidDownloadIntentSuccessInput reports a malformed new-task snapshot.
	ErrInvalidDownloadIntentSuccessInput = errors.New("下载成功通知输入无效")
	// ErrDownloadIntentSuccessInputNotFound hides missing and other-owner inputs.
	ErrDownloadIntentSuccessInputNotFound = errors.New("下载成功通知输入不存在")
	// ErrDownloadIntentSuccessInputConflict reports an immutable replay mismatch.
	ErrDownloadIntentSuccessInputConflict = errors.New("下载成功通知输入冲突")
	// ErrDownloadIntentSuccessInputPersistence is the sanitized storage boundary.
	ErrDownloadIntentSuccessInputPersistence = errors.New("下载成功通知输入持久化失败")
)

var errStoredDownloadIntentSuccessInputInvalid = errors.New("持久化下载成功通知输入无效")

const createDownloadIntentSuccessInputSQL = `
	INSERT INTO download_intent_success_inputs (
		intent_id, version, legacy, username, manual, background,
		media_source, media_id, title_year, media_type, category,
		vote_average, resource_term, poster_url, backdrop_url, episodes
	)
	SELECT $1,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17::jsonb
	FROM download_intent_plans AS p
	JOIN download_intents AS di ON di.id=p.intent_id
	WHERE p.intent_id=$1 AND di.username=$2
	ON CONFLICT (intent_id) DO NOTHING
	RETURNING intent_id, version, legacy, username, manual, background,
		media_source, media_id, title_year, media_type, category,
		vote_average, resource_term, poster_url, backdrop_url, episodes`

const getDownloadIntentSuccessInputSQL = `
	SELECT s.intent_id, s.version, s.legacy, s.username, s.manual, s.background,
		s.media_source, s.media_id, s.title_year, s.media_type, s.category,
		s.vote_average, s.resource_term, s.poster_url, s.backdrop_url, s.episodes
	FROM download_intent_success_inputs AS s
	JOIN download_intents AS di ON di.id=s.intent_id
	WHERE s.intent_id=$1 AND di.username=$2`

// CreateDownloadIntentSuccessInputTx atomically creates the immutable V1 input.
func (s *Store) CreateDownloadIntentSuccessInputTx(
	ctx context.Context,
	tx pgx.Tx,
	username string,
	input organization.SuccessInput,
) (organization.SuccessInput, error) {
	username, input, episodes, err := normalizeDownloadIntentSuccessInputCreate(username, input)
	if err != nil {
		return organization.SuccessInput{}, err
	}
	if s == nil || tx == nil || ctx == nil {
		return organization.SuccessInput{}, ErrDownloadIntentSuccessInputPersistence
	}
	return createDownloadIntentSuccessInput(ctx, tx, username, input, episodes)
}

// GetDownloadIntentSuccessInput returns one immutable input scoped to its owner.
func (s *Store) GetDownloadIntentSuccessInput(
	ctx context.Context,
	username string,
	intentID uuid.UUID,
) (organization.SuccessInput, error) {
	username, err := normalizeDownloadIntentSuccessInputOwner(username, intentID)
	if err != nil {
		return organization.SuccessInput{}, err
	}
	if s == nil || s.db == nil || s.db.Pool == nil || ctx == nil {
		return organization.SuccessInput{}, ErrDownloadIntentSuccessInputPersistence
	}
	return getDownloadIntentSuccessInput(ctx, s.db.Pool, username, intentID)
}

func createDownloadIntentSuccessInput(
	ctx context.Context,
	querier pgxQueryRower,
	username string,
	input organization.SuccessInput,
	episodes []byte,
) (organization.SuccessInput, error) {
	stored, err := scanDownloadIntentSuccessInput(querier.QueryRow(
		ctx,
		createDownloadIntentSuccessInputSQL,
		input.IntentID,
		username,
		input.Version,
		input.Legacy,
		input.Username,
		input.Manual,
		input.Background,
		input.Media.Source,
		input.Media.MediaID,
		input.Media.TitleYear,
		input.Media.MediaType,
		input.Media.Category,
		input.Media.VoteAverage,
		input.ResourceTerm,
		input.Media.PosterURL,
		input.Media.BackdropURL,
		episodes,
	))
	if err == nil {
		return stored, nil
	}
	if contextErr := downloadIntentSuccessInputContextError(ctx); contextErr != nil {
		return organization.SuccessInput{}, contextErr
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return organization.SuccessInput{}, ErrDownloadIntentSuccessInputPersistence
	}
	existing, err := getDownloadIntentSuccessInput(ctx, querier, username, input.IntentID)
	if err != nil {
		return organization.SuccessInput{}, err
	}
	if reflect.DeepEqual(existing, input) {
		return existing, nil
	}
	return organization.SuccessInput{}, ErrDownloadIntentSuccessInputConflict
}

func getDownloadIntentSuccessInput(
	ctx context.Context,
	querier pgxQueryRower,
	username string,
	intentID uuid.UUID,
) (organization.SuccessInput, error) {
	stored, err := scanDownloadIntentSuccessInput(querier.QueryRow(
		ctx, getDownloadIntentSuccessInputSQL, intentID, username,
	))
	if err == nil {
		return stored, nil
	}
	if contextErr := downloadIntentSuccessInputContextError(ctx); contextErr != nil {
		return organization.SuccessInput{}, contextErr
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return organization.SuccessInput{}, ErrDownloadIntentSuccessInputNotFound
	}
	return organization.SuccessInput{}, ErrDownloadIntentSuccessInputPersistence
}

func scanDownloadIntentSuccessInput(row rowScanner) (organization.SuccessInput, error) {
	var input organization.SuccessInput
	var episodes []byte
	if err := row.Scan(
		&input.IntentID,
		&input.Version,
		&input.Legacy,
		&input.Username,
		&input.Manual,
		&input.Background,
		&input.Media.Source,
		&input.Media.MediaID,
		&input.Media.TitleYear,
		&input.Media.MediaType,
		&input.Media.Category,
		&input.Media.VoteAverage,
		&input.ResourceTerm,
		&input.Media.PosterURL,
		&input.Media.BackdropURL,
		&episodes,
	); err != nil {
		return organization.SuccessInput{}, err
	}
	if err := json.Unmarshal(episodes, &input.Episodes); err != nil {
		return organization.SuccessInput{}, errStoredDownloadIntentSuccessInputInvalid
	}
	normalized, err := organization.NormalizeSuccessInput(input)
	if err != nil || !reflect.DeepEqual(normalized, input) {
		return organization.SuccessInput{}, errStoredDownloadIntentSuccessInputInvalid
	}
	return normalized, nil
}

func normalizeDownloadIntentSuccessInputCreate(
	username string,
	input organization.SuccessInput,
) (string, organization.SuccessInput, []byte, error) {
	username = strings.TrimSpace(username)
	normalized, err := organization.NormalizeSuccessInput(input)
	if err != nil || username == "" || normalized.Legacy ||
		normalized.Version != organization.CurrentSuccessVersion || normalized.Username != username {
		return "", organization.SuccessInput{}, nil, ErrInvalidDownloadIntentSuccessInput
	}
	if normalized.Episodes == nil {
		normalized.Episodes = make([]organization.Episode, 0)
	}
	episodes, err := json.Marshal(normalized.Episodes)
	if err != nil {
		return "", organization.SuccessInput{}, nil, ErrInvalidDownloadIntentSuccessInput
	}
	return username, normalized, episodes, nil
}

func normalizeDownloadIntentSuccessInputOwner(username string, intentID uuid.UUID) (string, error) {
	username = strings.TrimSpace(username)
	if username == "" || intentID == uuid.Nil {
		return "", ErrInvalidDownloadIntentSuccessInput
	}
	return username, nil
}

func downloadIntentSuccessInputContextError(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}
