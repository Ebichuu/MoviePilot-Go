package database

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/google/uuid"
)

func (s *Store) prepareRSSLocatorSecrets(items []RSSItemInput) ([]RSSItemInput, error) {
	prepared := make([]RSSItemInput, len(items))
	copy(prepared, items)
	for i := range prepared {
		secret := strings.TrimSpace(prepared[i].ResourceSecret)
		if secret == "" {
			prepared[i].ResourceSecret = ""
			continue
		}
		if len(secret) > maxRSSLocator || strings.ContainsAny(secret, "\r\n") {
			return nil, ErrInvalidRSSInput
		}
		if s == nil || s.secretBox == nil {
			return nil, ErrRSSPersistence
		}
		encrypted, err := s.secretBox.Encrypt(secret)
		if err != nil {
			return nil, ErrRSSPersistence
		}
		prepared[i].ResourceSecret = encrypted
	}
	return prepared, nil
}

func normalizeRSSItems(items []RSSItemInput) ([]RSSItemInput, error) {
	if len(items) > maxRSSItemsLimit {
		return nil, ErrInvalidRSSInput
	}
	result := make([]RSSItemInput, len(items))
	for i, item := range items {
		item.ItemKey = strings.TrimSpace(item.ItemKey)
		item.GUID = strings.TrimSpace(item.GUID)
		item.DownloadID = strings.TrimSpace(item.DownloadID)
		item.DetailURL = strings.TrimSpace(item.DetailURL)
		item.DownloadURL = strings.TrimSpace(item.DownloadURL)
		item.Title = strings.TrimSpace(item.Title)
		item.RecognizedMediaTitle = strings.TrimSpace(item.RecognizedMediaTitle)
		if len(item.ItemKey) < 8 || len(item.ItemKey) > maxRSSItemKey || item.Title == "" || len(item.Title) > maxRSSTitle ||
			len(item.GUID) > maxRSSGUID || len(item.DownloadID) > maxRSSLocator || len(item.DetailURL) > maxRSSLocator || len(item.DownloadURL) > maxRSSLocator ||
			len(item.RecognizedMediaTitle) > maxRSSTitle || item.MediaRecognizeFailCount < 0 || item.MediaRecognizeFailCount > maxRSSMediaRecognizeFailures {
			return nil, ErrInvalidRSSInput
		}
		if item.RecognizedMediaTitle != "" {
			item.MediaRecognizeFailCount = 0
		}
		if containsRSSSecretMarker(item.GUID) || containsRSSSecretMarker(item.DownloadID) ||
			containsRSSSecretMarker(item.DetailURL) || containsRSSSecretMarker(item.DownloadURL) {
			return nil, ErrInvalidRSSInput
		}
		if item.PublishedAt != nil {
			published := item.PublishedAt.UTC()
			item.PublishedAt = &published
		}
		resource, err := normalizeRSSResource(item.Resource)
		if err != nil {
			return nil, err
		}
		item.Resource = resource
		result[i] = item
	}
	return result, nil
}

func normalizeRSSResource(resource json.RawMessage) (json.RawMessage, error) {
	if len(resource) == 0 {
		return json.RawMessage(`{}`), nil
	}
	if len(resource) > maxRSSResourceBytes || !json.Valid(resource) {
		return nil, ErrInvalidRSSInput
	}
	var object map[string]any
	if err := json.Unmarshal(resource, &object); err != nil || object == nil {
		return nil, ErrInvalidRSSInput
	}
	if containsSensitiveRSSValue(object) {
		return nil, ErrInvalidRSSInput
	}
	encoded, err := json.Marshal(object)
	if err != nil || len(encoded) > maxRSSResourceBytes {
		return nil, ErrInvalidRSSInput
	}
	return encoded, nil
}

func rssCacheSignature(title, description string) string {
	return title + description
}

func filterRSSItemsByCacheSignatures(cached map[string]struct{}, items []RSSItemInput) []RSSItemInput {
	if len(cached) == 0 || len(items) == 0 {
		return items
	}
	filtered := make([]RSSItemInput, 0, len(items))
	for _, item := range items {
		var resource struct {
			Description string `json:"description"`
		}
		if err := json.Unmarshal(item.Resource, &resource); err != nil {
			filtered = append(filtered, item)
			continue
		}
		if _, exists := cached[rssCacheSignature(item.Title, resource.Description)]; !exists {
			filtered = append(filtered, item)
		}
	}
	return filtered
}

func scanRSSCursor(row rowScanner) (RSSCursor, error) {
	var cursor RSSCursor
	var leaseToken *uuid.UUID
	if err := row.Scan(&cursor.SiteID, &cursor.Endpoint, &cursor.ETag, &cursor.LastModified,
		&cursor.HighWaterPublishedAt, &cursor.HighWaterItemKey, &cursor.NextPollAt,
		&cursor.LastAttemptAt, &cursor.LastSuccessAt, &cursor.FailureCount,
		&cursor.LastError, &leaseToken, &cursor.LeaseExpiresAt, &cursor.UpdatedAt); err != nil {
		return RSSCursor{}, err
	}
	if leaseToken != nil {
		cursor.LeaseToken = *leaseToken
	}
	cursor.NextPollAt = cursor.NextPollAt.UTC()
	cursor.UpdatedAt = cursor.UpdatedAt.UTC()
	return cursor, nil
}

func (s *Store) scanRSSItem(row rowScanner) (RSSItem, error) {
	var item RSSItem
	var leaseToken *uuid.UUID
	var encryptedSecret string
	if err := row.Scan(&item.ID, &item.SiteID, &item.ItemKey, &item.GUID, &item.DownloadID,
		&encryptedSecret, &item.DetailURL, &item.DownloadURL, &item.Title, &item.RecognizedMediaTitle,
		&item.MediaRecognizeFailCount, &item.PublishedAt, &item.Resource,
		&item.FirstSeenAt, &item.LastSeenAt, &item.State, &item.AttemptCount,
		&item.LastError, &item.MatchedAt, &leaseToken, &item.LeaseExpires, &item.UpdatedAt); err != nil {
		return RSSItem{}, err
	}
	if encryptedSecret != "" {
		if s == nil || s.secretBox == nil {
			return RSSItem{}, ErrRSSPersistence
		}
		secret, err := s.secretBox.Decrypt(encryptedSecret)
		if err != nil || len(secret) > maxRSSLocator || strings.ContainsAny(secret, "\r\n") {
			return RSSItem{}, ErrRSSPersistence
		}
		item.ResourceSecret = secret
	}
	if leaseToken != nil {
		item.LeaseToken = *leaseToken
	}
	if len(item.Resource) > maxRSSResourceBytes || !json.Valid(item.Resource) {
		return RSSItem{}, ErrInvalidRSSInput
	}
	item.FirstSeenAt = item.FirstSeenAt.UTC()
	item.LastSeenAt = item.LastSeenAt.UTC()
	item.UpdatedAt = item.UpdatedAt.UTC()
	return item, nil
}

func containsRSSSecretMarker(value string) bool {
	lower := strings.ToLower(value)
	for _, marker := range []string{
		"cookie=", "cookie:", "cookie ", "passkey=", "passkey:", "passkey ",
		"authorization=", "authorization:", "authorization ", "apikey=", "apikey:", "apikey ",
		"api_key=", "api_key:", "api_key ", "token=", "token:", "token ", "secret=", "secret:", "secret ",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func containsSensitiveRSSValue(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, nested := range typed {
			switch strings.ToLower(strings.TrimSpace(key)) {
			case "cookie", "headers", "passkey", "apikey", "api_key", "token", "secret", "authorization":
				return true
			}
			if containsSensitiveRSSValue(nested) {
				return true
			}
		}
	case []any:
		for _, nested := range typed {
			if containsSensitiveRSSValue(nested) {
				return true
			}
		}
	case string:
		return containsRSSSecretMarker(typed)
	}
	return false
}

func validateRSSStore(store *Store, ctx context.Context) error {
	if ctx == nil {
		return ErrRSSPersistence
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if store == nil || store.db == nil || store.db.Pool == nil {
		return ErrRSSPersistence
	}
	return nil
}

func rssPersistenceError(ctx context.Context) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return ErrRSSPersistence
}

func cleanRSSHeader(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 512 {
		return value[:512]
	}
	return value
}

func sanitizeRSSError(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if containsRSSSecretMarker(value) {
		return "rss operation failed"
	}
	var builder strings.Builder
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			continue
		}
		builder.WriteRune(r)
		if builder.Len() >= maxRSSLastError {
			break
		}
	}
	return builder.String()
}
