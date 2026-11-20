package database

import (
	"context"
	"encoding/json"
	"slices"
)

const getSubscriptionRSSSitesSQL = `
	SELECT
		COUNT(*) > 0,
		COALESCE(
			jsonb_agg(config->'sites' ORDER BY id)
				FILTER (WHERE v2_state IN ('R', 'P')),
			'[]'::jsonb
		)
	FROM subscriptions
	WHERE completed_at IS NULL`

// SubscriptionRSSSitesSnapshot 是一次 RSS 刷新所需的最小订阅站点快照。
// SiteSelections 为 R/P 订阅各自的站点选择；空选择必须保留，以便业务层复现 V2 回退语义。
type SubscriptionRSSSitesSnapshot struct {
	HasSubscriptions bool
	SiteSelections   [][]int
}

// GetSubscriptionRSSSites 一次读取未完成订阅的存在性及 R/P 订阅的独立站点选择。
func (s *Store) GetSubscriptionRSSSites(ctx context.Context) (SubscriptionRSSSitesSnapshot, error) {
	empty := SubscriptionRSSSitesSnapshot{SiteSelections: [][]int{}}
	if ctx == nil {
		return empty, ErrSubscriptionPreciseSearchPersistence
	}
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	if !subscriptionPreciseSearchStoreReady(s) {
		return empty, ErrSubscriptionPreciseSearchPersistence
	}

	snapshot, err := scanSubscriptionRSSSites(s.db.Pool.QueryRow(ctx, getSubscriptionRSSSitesSQL))
	if err != nil {
		return empty, subscriptionPreciseSearchPersistenceError(ctx)
	}
	return snapshot, nil
}

type subscriptionRSSSitesRow interface {
	Scan(...any) error
}

func scanSubscriptionRSSSites(row subscriptionRSSSitesRow) (SubscriptionRSSSitesSnapshot, error) {
	snapshot := SubscriptionRSSSitesSnapshot{SiteSelections: [][]int{}}
	var encoded []byte
	if err := row.Scan(&snapshot.HasSubscriptions, &encoded); err != nil {
		return SubscriptionRSSSitesSnapshot{}, err
	}
	snapshot.SiteSelections = decodeSubscriptionRSSSiteSelections(encoded)
	return snapshot, nil
}

func decodeSubscriptionRSSSiteSelections(encoded []byte) [][]int {
	var rawSelections []json.RawMessage
	if err := json.Unmarshal(encoded, &rawSelections); err != nil {
		return [][]int{}
	}

	selections := make([][]int, 0, len(rawSelections))
	for _, rawSelection := range rawSelections {
		var values []any
		if err := json.Unmarshal(rawSelection, &values); err != nil {
			selections = append(selections, []int{})
			continue
		}

		siteIDs := make([]int, 0, len(values))
		for _, value := range values {
			if siteID, ok := subscriptionInteger(value); ok {
				siteIDs = append(siteIDs, siteID)
			}
		}
		slices.Sort(siteIDs)
		siteIDs = slices.Compact(siteIDs)
		selections = append(selections, siteIDs)
	}

	slices.SortFunc(selections, func(left, right []int) int {
		return slices.Compare(left, right)
	})
	selections = slices.CompactFunc(selections, slices.Equal)
	if selections == nil {
		return [][]int{}
	}
	return selections
}
