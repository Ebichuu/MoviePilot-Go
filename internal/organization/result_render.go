package organization

import "github.com/google/uuid"

// V2ManualFailure is the default V2 per-file organization failure message.
type V2ManualFailure struct {
	Title    string
	Text     string
	Image    string
	Link     *string
	Username string
}

// Clone returns a rendering that does not share its optional link pointer.
func (m V2ManualFailure) Clone() V2ManualFailure {
	if m.Link != nil {
		link := *m.Link
		m.Link = &link
	}
	return m
}

// RenderV2OrganizeResult renders a successful media-and-season subset. When
// the group's last terminal file failed, V2 appends only that failure text.
func RenderV2OrganizeResult(result ResultSnapshot, appDomain string) (V2OrganizeSuccess, error) {
	normalized, err := NormalizeSuccessSnapshot(result.Snapshot)
	if err != nil || result.TerminalOrdinal < 0 || !validResultError(result.ErrMsg) {
		return V2OrganizeSuccess{}, ErrInvalidTerminalResults
	}
	maxOrdinal := -1
	for _, file := range normalized.Files {
		if file.Ordinal > maxOrdinal {
			maxOrdinal = file.Ordinal
		}
	}
	if result.TerminalOrdinal < maxOrdinal {
		return V2OrganizeSuccess{}, ErrInvalidTerminalResults
	}

	rendered, err := RenderV2OrganizeSuccess(normalized, appDomain)
	if err != nil {
		return V2OrganizeSuccess{}, err
	}
	rendered.Context.ErrMsg = result.ErrMsg
	if result.ErrMsg != "" {
		rendered.Text += "，以下文件处理失败：" + result.ErrMsg
	}
	return rendered, nil
}

// RenderV2ManualFailure renders V2's immediate Manual notification for one
// terminal file failure. Transfer-history retry controls are intentionally not
// included because the Go backend does not yet own a V2 transfer-history row.
func RenderV2ManualFailure(failure FileFailure, appDomain string) (V2ManualFailure, error) {
	normalized, err := normalizeFileFailure(failure)
	if err != nil {
		return V2ManualFailure{}, err
	}
	seasonEpisode := ""
	if normalized.Snapshot.Media.MediaType == "电视剧" {
		seasonEpisode = FormatV2SeasonEpisodes(normalized.File.Season, normalized.File.Episodes)
	}
	return V2ManualFailure{
		Title:    normalized.Snapshot.Media.TitleYear + " " + seasonEpisode + " 入库失败！",
		Text:     "原因：" + normalized.Error,
		Image:    V2MessageImage(normalized.Snapshot.Media),
		Link:     V2HistoryLink(appDomain),
		Username: normalized.Snapshot.Username,
	}, nil
}

func normalizeFileFailure(failure FileFailure) (FileFailure, error) {
	context := failure.Snapshot.Clone()
	if context.BatchID == uuid.Nil || context.IntentID == uuid.Nil ||
		!validVersion(context.Version, context.Legacy) || !validGroupKey(context.GroupKey) ||
		!validFileOutcomeError(failure.Error) {
		return FileFailure{}, ErrInvalidTerminalResults
	}
	var err error
	if context.Username, err = normalizeText(context.Username, maxUsernameBytes); err != nil || context.Username == "" {
		return FileFailure{}, ErrInvalidTerminalResults
	}
	if context.Media, err = NormalizeMediaContext(context.Media); err != nil {
		return FileFailure{}, ErrInvalidTerminalResults
	}
	file, err := NormalizeFileFact(failure.File)
	if err != nil {
		return FileFailure{}, ErrInvalidTerminalResults
	}
	groupKey, err := BuildGroupKey(context.Media, file.Season)
	if err != nil || groupKey != context.GroupKey {
		return FileFailure{}, ErrInvalidTerminalResults
	}
	return FileFailure{Snapshot: context, File: file, Error: failure.Error}, nil
}

func validResultError(value string) bool {
	return value == "" || validFileOutcomeError(value)
}
