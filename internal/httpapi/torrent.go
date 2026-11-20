package httpapi

import "github.com/moviepilot-go/moviepilot-go/internal/torrentmeta"

func validateTorrentMetainfo(body []byte) error {
	_, err := torrentmeta.Inspect(body)
	return err
}
