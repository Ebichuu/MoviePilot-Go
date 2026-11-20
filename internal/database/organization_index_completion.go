package database

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/mediaindex"
	"github.com/moviepilot-go/moviepilot-go/internal/notificationflow"
	"github.com/moviepilot-go/moviepilot-go/internal/scrapeflow"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

type mediaIndexOrganizationFile struct {
	Ordinal        int
	TargetPath     string
	ExpectedSize   int64
	ExpectedSHA256 string
}

// CompleteOrganizationBatchForIndexing 原子完成整理批次；required server
// 只创建持久确认请求，不延后订阅结算。
func (s *Store) CompleteOrganizationBatchForIndexing(
	ctx context.Context,
	batchID uuid.UUID,
	token uuid.UUID,
	plan mediaindex.CompletionPlan,
) (uuid.UUID, bool, error) {
	requestID, created, _, _, err := s.CompleteOrganizationBatchWithSideEffects(
		ctx,
		batchID,
		token,
		plan,
		scrapeflow.Plan{},
		notificationflow.Plan{},
	)
	return requestID, created, err
}

func normalizeMediaIndexCompletionPlan(plan mediaindex.CompletionPlan) (mediaindex.CompletionPlan, uuid.UUID, error) {
	plan = plan.Clone()
	if plan.RequiredServer == nil || len(plan.Files) < 1 || len(plan.Files) > maxOrganizationPlanFiles {
		return mediaindex.CompletionPlan{}, uuid.Nil, mediaindex.ErrInvalidRequest
	}
	serverID, err := uuid.Parse(strings.TrimSpace(plan.RequiredServer.ID))
	if err != nil {
		return mediaindex.CompletionPlan{}, uuid.Nil, mediaindex.ErrInvalidRequest
	}
	plan.RequiredServer.ID = serverID.String()
	plan.RequiredServer.Name = strings.TrimSpace(plan.RequiredServer.Name)
	plan.RequiredServer.Type = strings.ToLower(strings.TrimSpace(plan.RequiredServer.Type))
	plan.RequiredServer.Fingerprint = strings.TrimSpace(plan.RequiredServer.Fingerprint)
	if plan.RequiredServer.Name == "" || plan.RequiredServer.Type != "emby" || len(plan.RequiredServer.Name) > 255 ||
		!validOrganizationSHA256(plan.RequiredServer.Fingerprint) {
		return mediaindex.CompletionPlan{}, uuid.Nil, mediaindex.ErrInvalidRequest
	}
	remotePaths := make(map[string]struct{}, len(plan.Files))
	for index := range plan.Files {
		file := &plan.Files[index]
		file.LocalPath = strings.TrimSpace(file.LocalPath)
		file.RemotePath = strings.TrimSpace(file.RemotePath)
		file.PathStyle = strings.ToLower(strings.TrimSpace(file.PathStyle))
		file.MediaType = strings.TrimSpace(file.MediaType)
		file.ProviderID = strings.TrimSpace(file.ProviderID)
		if file.LocalPath == "" || len(file.LocalPath) > maxMediaIndexPath || strings.IndexByte(file.LocalPath, 0) >= 0 ||
			file.RemotePath == "" || len(file.RemotePath) > maxMediaIndexPath || strings.IndexByte(file.RemotePath, 0) >= 0 ||
			(file.PathStyle != "posix" && file.PathStyle != "windows") || file.ExpectedSize < 0 ||
			!validOrganizationSHA256(file.ExpectedSHA256) || file.MediaType == "" || len(file.MediaType) > 64 ||
			file.Season < 0 || file.Season > 999 || file.Episode < 0 || file.Episode > 5000 ||
			len(file.ProviderID) > maxMediaIndexID || strings.ContainsAny(file.ProviderID, "\x00\r\n") {
			return mediaindex.CompletionPlan{}, uuid.Nil, mediaindex.ErrInvalidRequest
		}
		remoteKey := canonicalMediaIndexRemotePath(file.RemotePath, file.PathStyle, file.CaseSensitive)
		if _, exists := remotePaths[remoteKey]; exists {
			return mediaindex.CompletionPlan{}, uuid.Nil, mediaindex.ErrInvalidRequest
		}
		remotePaths[remoteKey] = struct{}{}
	}
	return plan, serverID, nil
}

func loadCompletedOrganizationFilesForIndex(ctx context.Context, tx pgx.Tx, batchID uuid.UUID) ([]mediaIndexOrganizationFile, error) {
	rows, err := tx.Query(ctx, `
		SELECT ordinal, target_path, expected_size, expected_sha256
		FROM organization_files
		WHERE batch_id=$1 AND state='completed'
		ORDER BY ordinal
		FOR SHARE`, batchID)
	if err != nil {
		return nil, organizationPersistenceError(ctx)
	}
	defer rows.Close()
	items := make([]mediaIndexOrganizationFile, 0)
	for rows.Next() {
		var item mediaIndexOrganizationFile
		if err := rows.Scan(&item.Ordinal, &item.TargetPath, &item.ExpectedSize, &item.ExpectedSHA256); err != nil {
			return nil, organizationPersistenceError(ctx)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, organizationPersistenceError(ctx)
	}
	var total, terminal int
	if err := tx.QueryRow(ctx, `
		SELECT count(*), count(*) FILTER (WHERE state IN ('completed', 'terminal_failed'))
		FROM organization_files
		WHERE batch_id=$1`, batchID).Scan(&total, &terminal); err != nil {
		return nil, organizationPersistenceError(ctx)
	}
	if total < 1 || terminal != total {
		return nil, ErrOrganizationFilesIncomplete
	}
	return items, nil
}

func matchMediaIndexFiles(stored []mediaIndexOrganizationFile, planned []mediaindex.TargetFile) error {
	if len(planned) < 1 || len(stored) < len(planned) {
		return ErrOrganizationStateConflict
	}
	matched := make([]bool, len(stored))
	for _, target := range planned {
		matchIndex := -1
		for index, file := range stored {
			if file.TargetPath != target.LocalPath || file.ExpectedSize != target.ExpectedSize ||
				file.ExpectedSHA256 != target.ExpectedSHA256 {
				continue
			}
			if matchIndex >= 0 {
				return ErrOrganizationStateConflict
			}
			matchIndex = index
		}
		if matchIndex < 0 || matched[matchIndex] {
			return ErrOrganizationStateConflict
		}
		matched[matchIndex] = true
	}
	return nil
}

func insertMediaIndexRequest(
	ctx context.Context,
	tx pgx.Tx,
	requestID, batchID, serverID uuid.UUID,
	plan mediaindex.CompletionPlan,
) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO media_index_requests (
			id, batch_id, server_id, server_name, server_type, server_fingerprint, expected_file_count,
			state, next_attempt_at, last_error
		) VALUES ($1, $2, $3, $4, $5, $6, $7, 'pending', now(), '')`,
		requestID, batchID, serverID, plan.RequiredServer.Name, plan.RequiredServer.Type,
		plan.RequiredServer.Fingerprint, len(plan.Files)); err != nil {
		return organizationPersistenceError(ctx)
	}
	for ordinal, file := range plan.Files {
		if _, err := tx.Exec(ctx, `
			INSERT INTO media_index_files (
				request_id, ordinal, local_path, remote_path, path_style, case_sensitive,
				expected_size, expected_sha256, media_type, season, episode, provider_id
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
			requestID, ordinal, file.LocalPath, file.RemotePath, file.PathStyle, file.CaseSensitive,
			file.ExpectedSize, file.ExpectedSHA256, file.MediaType, file.Season, file.Episode, file.ProviderID); err != nil {
			return organizationPersistenceError(ctx)
		}
	}
	return nil
}

func verifyMediaIndexRequestPlan(
	ctx context.Context,
	tx pgx.Tx,
	requestID, batchID, serverID uuid.UUID,
	plan mediaindex.CompletionPlan,
) error {
	var name, serverType, serverFingerprint string
	var expectedFileCount int
	if err := tx.QueryRow(ctx, `
		SELECT server_name, server_type, server_fingerprint, expected_file_count
		FROM media_index_requests
		WHERE id=$1 AND batch_id=$2 AND server_id=$3`, requestID, batchID, serverID).Scan(
		&name, &serverType, &serverFingerprint, &expectedFileCount,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrOrganizationStateConflict
		}
		return organizationPersistenceError(ctx)
	}
	if name != plan.RequiredServer.Name || serverType != plan.RequiredServer.Type ||
		serverFingerprint != plan.RequiredServer.Fingerprint || expectedFileCount != len(plan.Files) {
		return ErrOrganizationStateConflict
	}
	rows, err := tx.Query(ctx, `
		SELECT local_path, remote_path, path_style, case_sensitive, expected_size,
			expected_sha256, media_type, season, episode, provider_id
		FROM media_index_files WHERE request_id=$1 ORDER BY ordinal`, requestID)
	if err != nil {
		return organizationPersistenceError(ctx)
	}
	defer rows.Close()
	index := 0
	for rows.Next() {
		if index >= len(plan.Files) {
			return ErrOrganizationStateConflict
		}
		var stored mediaindex.TargetFile
		if err := rows.Scan(
			&stored.LocalPath, &stored.RemotePath, &stored.PathStyle, &stored.CaseSensitive,
			&stored.ExpectedSize, &stored.ExpectedSHA256, &stored.MediaType,
			&stored.Season, &stored.Episode, &stored.ProviderID,
		); err != nil {
			return organizationPersistenceError(ctx)
		}
		if stored != plan.Files[index] {
			return ErrOrganizationStateConflict
		}
		index++
	}
	if err := rows.Err(); err != nil {
		return organizationPersistenceError(ctx)
	}
	if index != len(plan.Files) {
		return ErrOrganizationStateConflict
	}
	return nil
}

func mediaIndexRequestID(batchID, serverID uuid.UUID) uuid.UUID {
	return uuid.NewSHA1(batchID, []byte(serverID.String()))
}

func canonicalMediaIndexRemotePath(value, style string, caseSensitive bool) string {
	value = strings.TrimSpace(value)
	if style == "windows" {
		value = strings.ReplaceAll(value, "/", `\`)
		for strings.Contains(value, `\\`) {
			value = strings.ReplaceAll(value, `\\`, `\`)
		}
		value = strings.TrimRight(value, `\`)
	} else {
		value = strings.ReplaceAll(value, `\`, "/")
		for strings.Contains(value, "//") {
			value = strings.ReplaceAll(value, "//", "/")
		}
		if value != "/" {
			value = strings.TrimRight(value, "/")
		}
	}
	if !caseSensitive {
		value = strings.ToLower(value)
	}
	return style + "\x00" + value
}

func completeLegacyPendingIndexSubscription(ctx context.Context, tx pgx.Tx, batchID uuid.UUID) error {
	var intentID uuid.UUID
	if err := tx.QueryRow(ctx, `
		SELECT intent_id FROM organization_batches WHERE id=$1 FOR UPDATE`, batchID).Scan(&intentID); err != nil {
		return organizationPersistenceError(ctx)
	}
	var totalRequests, confirmedRequests int64
	if err := tx.QueryRow(ctx, `
		SELECT count(*), count(*) FILTER (WHERE state='confirmed')
		FROM media_index_requests WHERE batch_id=$1`, batchID).Scan(&totalRequests, &confirmedRequests); err != nil {
		return mediaIndexPersistenceError(ctx)
	}
	if totalRequests < 1 || totalRequests != confirmedRequests {
		return nil
	}
	var subscriptionID *int64
	if err := tx.QueryRow(ctx, `SELECT subscription_id FROM download_intents WHERE id=$1`, intentID).Scan(&subscriptionID); err != nil {
		return organizationPersistenceError(ctx)
	}
	if subscriptionID == nil {
		return nil
	}
	var (
		state         subscription.State
		currentIntent *uuid.UUID
		completed     bool
	)
	if err := tx.QueryRow(ctx, `
		SELECT internal_state, current_download_intent_id, completed_at IS NOT NULL
		FROM subscriptions WHERE id=$1 FOR UPDATE`, *subscriptionID).Scan(&state, &currentIntent, &completed); err != nil {
		return organizationPersistenceError(ctx)
	}
	if completed || state != subscription.PendingIndex || currentIntent == nil || *currentIntent != intentID {
		return nil
	}
	return completeOrganizationSubscription(ctx, tx, *subscriptionID, intentID)
}
