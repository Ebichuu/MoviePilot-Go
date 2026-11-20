package database

import "github.com/moviepilot-go/moviepilot-go/internal/subscription"

func scanSubscriptionEpisodeSummary(row rowScanner) (subscription.SubscriptionEpisodeSummary, error) {
	counts := make([]int64, 8)
	if err := row.Scan(
		&counts[0],
		&counts[1],
		&counts[2],
		&counts[3],
		&counts[4],
		&counts[5],
		&counts[6],
		&counts[7],
	); err != nil {
		return subscription.SubscriptionEpisodeSummary{}, err
	}
	if counts[0] > maxSubscriptionEpisodes || sumInt64(counts[1:]) != counts[0] {
		return subscription.SubscriptionEpisodeSummary{}, ErrSubscriptionEpisodePersistence
	}
	return subscription.SubscriptionEpisodeSummary{
		Total:               int(counts[0]),
		Missing:             int(counts[1]),
		Acquired:            int(counts[2]),
		Downloading:         int(counts[3]),
		PendingOrganization: int(counts[4]),
		Organizing:          int(counts[5]),
		PendingIndex:        int(counts[6]),
		Indexed:             int(counts[7]),
	}, nil
}

func scanSubscriptionEpisode(row rowScanner) (subscription.SubscriptionEpisode, error) {
	var item subscription.SubscriptionEpisode
	if err := row.Scan(
		&item.ID,
		&item.SubscriptionID,
		&item.Season,
		&item.Episode,
		&item.State,
		&item.DownloadIntentID,
		&item.UpdatedAt,
	); err != nil {
		return subscription.SubscriptionEpisode{}, err
	}
	if item.ID <= 0 || item.SubscriptionID <= 0 ||
		!validSubscriptionEpisode(subscription.Episode{Season: item.Season, Episode: item.Episode}) ||
		!validSubscriptionEpisodeState(item.State) {
		return subscription.SubscriptionEpisode{}, ErrSubscriptionEpisodePersistence
	}
	return item, nil
}

func sumInt64(values []int64) int64 {
	var total int64
	for _, value := range values {
		total += value
	}
	return total
}
