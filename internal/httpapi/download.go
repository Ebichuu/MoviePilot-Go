package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/downloadconfig"
	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
	"github.com/moviepilot-go/moviepilot-go/internal/downloadflow"
	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
	"github.com/moviepilot-go/moviepilot-go/internal/media"
	"github.com/moviepilot-go/moviepilot-go/internal/organization"
)

const (
	directoriesSettingKey           = "Directories"
	defaultDownloadRequestBodyLimit = int64(1 << 20)
)

// DownloadSubmission 是下载 HTTP 边界消费的原子提交契约。
type DownloadSubmission interface {
	Submit(
		ctx context.Context,
		username string,
		intent downloader.Intent,
		plan database.DownloadIntentPlan,
		successInput organization.SuccessInput,
	) (uuid.UUID, error)
}

// DownloadRequestStore 是下载请求解析所需的最小服务端设置契约。
type DownloadRequestStore interface {
	LoadDownloaders(context.Context) ([]downloadconfig.Downloader, bool, error)
	GetSetting(context.Context, string, any) (bool, error)
}

// NamedDownloadTask 是下载器运行时向 HTTP 边界提供的带实例名任务快照。
type NamedDownloadTask struct {
	Downloader string
	Task       downloader.Task
}

// DownloadTaskSource 是 HTTP 下载列表消费的有界只读查询契约。
type DownloadTaskSource interface {
	ListDownloading(context.Context, string) ([]NamedDownloadTask, error)
}

// DownloadHistorySource 是下载历史消费的只读契约：仅下载器支持时才会被使用。
type DownloadHistorySource interface {
	ListDownloadHistory(context.Context, string) ([]NamedDownloadTask, error)
}

// DownloadIntentOwnerStore 批量解析确定性任务标签的持久化 owner。
type DownloadIntentOwnerStore interface {
	ListDownloadIntentOwners(context.Context, []string) (map[string]database.DownloadIntentOwner, error)
}

type downloadRequest struct {
	TorrentIn  downloadTorrentInput `json:"torrent_in"`
	MediaIn    downloadMediaInput   `json:"media_in,omitempty"`
	Downloader string               `json:"downloader"`
	SavePath   string               `json:"save_path"`
}

// downloadTorrentInput 故意不声明 Cookie、UA、代理及连接字段。
// json.Decoder 会忽略这些浏览器字段，下载凭据只从服务端设置恢复。
type downloadTorrentInput struct {
	Site           int    `json:"site"`
	SiteDownloader string `json:"site_downloader"`
	Title          string `json:"title"`
	Description    string `json:"description"`
	Enclosure      string `json:"enclosure"`
}

// downloadMediaInput 只接收 V2 下载入口构建整理成功通知所需的媒体字段。
// MediaInfo 的其余字段和浏览器附带的未知字段由 encoding/json 兼容性忽略。
type downloadMediaInput struct {
	Source       string  `json:"source"`
	MediaID      string  `json:"media_id"`
	MediaType    string  `json:"type"`
	Title        string  `json:"title"`
	Year         string  `json:"year"`
	TitleYear    string  `json:"title_year"`
	Season       *int    `json:"season"`
	TMDBID       int64   `json:"tmdb_id"`
	DoubanID     string  `json:"douban_id"`
	BangumiID    int64   `json:"bangumi_id"`
	AniListID    int64   `json:"anilist_id"`
	PosterPath   string  `json:"poster_path"`
	BackdropPath string  `json:"backdrop_path"`
	VoteAverage  float64 `json:"vote_average"`
	Category     string  `json:"category"`
}

type downloadDirectorySetting = downloadconfig.Directory

type downloadDirectoryView struct {
	Name          *string `json:"name"`
	Storage       string  `json:"storage"`
	DownloadPath  string  `json:"download_path"`
	SavePath      string  `json:"save_path"`
	Priority      int     `json:"priority"`
	MediaType     *string `json:"media_type"`
	MediaCategory *string `json:"media_category"`
}

type downloadTorrentView struct {
	Downloader       string         `json:"downloader"`
	Hash             string         `json:"hash"`
	Title            string         `json:"title"`
	SiteName         string         `json:"site_name"`
	Name             string         `json:"name"`
	Year             string         `json:"year"`
	SeasonEpisode    string         `json:"season_episode"`
	Path             string         `json:"path"`
	Size             int64          `json:"size"`
	Progress         float64        `json:"progress"`
	State            string         `json:"state"`
	UpSpeed          string         `json:"upspeed"`
	DownSpeed        string         `json:"dlspeed"`
	Tags             string         `json:"tags"`
	SavePath         string         `json:"save_path"`
	ContentPath      string         `json:"content_path"`
	Category         string         `json:"category"`
	DownloadLimit    float64        `json:"download_limit"`
	UploadLimit      float64        `json:"upload_limit"`
	RatioLimit       float64        `json:"ratio_limit"`
	SeedingTimeLimit int64          `json:"seeding_time_limit"`
	Trackers         []string       `json:"trackers"`
	Media            map[string]any `json:"media"`
	UserID           string         `json:"userid"`
	Username         string         `json:"username"`
	LeftTime         string         `json:"left_time"`
}

func (d Dependencies) listDownloads(w http.ResponseWriter, r *http.Request) {
	user, ok := d.authorizedUser(r)
	if !ok {
		writeError(w, http.StatusForbidden, "用户身份无效")
		return
	}
	if d.DownloadTasks == nil {
		writeError(w, http.StatusInternalServerError, "下载任务查询暂不可用")
		return
	}

	tasks, err := d.DownloadTasks.ListDownloading(r.Context(), strings.TrimSpace(r.URL.Query().Get("name")))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "查询下载任务失败")
		return
	}
	labels := downloadTaskLabels(tasks)
	ownerStore := d.downloadIntentOwnerStore()
	if len(labels) > 0 && ownerStore == nil {
		writeError(w, http.StatusInternalServerError, "查询下载任务失败")
		return
	}
	owners := map[string]database.DownloadIntentOwner{}
	if len(labels) > 0 {
		owners, err = ownerStore.ListDownloadIntentOwners(r.Context(), labels)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "查询下载任务失败")
			return
		}
	}

	items := make([]downloadTorrentView, 0, len(tasks))
	for _, named := range tasks {
		owner, found := owners[named.Task.Label]
		if !found || (!user.SuperUser && owner.UserID != user.ID) {
			continue
		}
		items = append(items, projectDownloadTorrent(named, owner))
	}
	writeJSON(w, http.StatusOK, items)
}

func (d Dependencies) downloadIntentOwnerStore() DownloadIntentOwnerStore {
	if d.DownloadOwners != nil {
		return d.DownloadOwners
	}
	if d.Store != nil {
		return d.Store
	}
	return nil
}

func downloadTaskLabels(tasks []NamedDownloadTask) []string {
	labels := make([]string, 0, len(tasks))
	seen := make(map[string]struct{}, len(tasks))
	for _, named := range tasks {
		label := strings.TrimSpace(named.Task.Label)
		if label == "" {
			continue
		}
		if _, exists := seen[label]; exists {
			continue
		}
		seen[label] = struct{}{}
		labels = append(labels, label)
	}
	return labels
}

func projectDownloadTorrent(named NamedDownloadTask, owner database.DownloadIntentOwner) downloadTorrentView {
	task := named.Task
	parsed := media.Parse(task.Title, media.Options{})
	progress := task.ProgressPercent
	if progress < 0 {
		progress = 0
	} else if progress > 100 {
		progress = 100
	}
	state := strings.TrimSpace(task.State)
	if state == "" {
		state = downloader.TaskStateDownloading
	}
	return downloadTorrentView{
		Downloader:       strings.TrimSpace(named.Downloader),
		Hash:             task.Hash,
		Title:            task.Title,
		SiteName:         "",
		Name:             parsed.Name(),
		Year:             parsed.Year,
		SeasonEpisode:    parsed.Season() + parsed.Episode(),
		Path:             task.ContentPath,
		Size:             task.SizeBytes,
		Progress:         progress,
		State:            state,
		UpSpeed:          formatDownloadSpeed(task.UpSpeedBytes),
		DownSpeed:        formatDownloadSpeed(task.DownSpeedBytes),
		Tags:             publicDownloadTags(task.Tags, task.Label),
		SavePath:         task.SavePath,
		ContentPath:      task.ContentPath,
		Category:         task.Category,
		DownloadLimit:    float64(task.DownloadLimitBytes) / 1024,
		UploadLimit:      float64(task.UploadLimitBytes) / 1024,
		RatioLimit:       task.RatioLimit,
		SeedingTimeLimit: task.SeedingTimeLimit,
		Trackers:         []string{},
		Media:            map[string]any{},
		UserID:           strconv.FormatInt(owner.UserID, 10),
		Username:         owner.Username,
		LeftTime:         formatDownloadDuration(task.ETASeconds),
	}
}

func formatDownloadSpeed(bytes int64) string {
	if bytes < 0 {
		bytes = 0
	}
	if bytes < 1024 {
		return strconv.FormatFloat(float64(bytes), 'f', 1, 64) + "B"
	}
	value := float64(bytes)
	unit := "K"
	for _, candidate := range []string{"K", "M", "G", "T"} {
		unit = candidate
		value /= 1024
		if value < 1024 || candidate == "T" {
			break
		}
	}
	formatted := strconv.FormatFloat(value, 'f', 2, 64)
	formatted = strings.TrimSuffix(formatted, "0")
	return formatted + unit
}

func formatDownloadDuration(seconds int64) string {
	if seconds <= 0 {
		return ""
	}
	hours := seconds / 3600
	minutes := seconds % 3600 / 60
	remaining := seconds % 60
	var value strings.Builder
	if hours > 0 {
		value.WriteString(strconv.FormatInt(hours, 10))
		value.WriteString("时")
	}
	if minutes > 0 {
		value.WriteString(strconv.FormatInt(minutes, 10))
		value.WriteString("分")
	}
	value.WriteString(strconv.FormatInt(remaining, 10))
	value.WriteString("秒")
	return value.String()
}

func publicDownloadTags(tags, internalLabel string) string {
	public := make([]string, 0)
	for _, raw := range strings.Split(tags, ",") {
		tag := strings.TrimSpace(raw)
		if tag == "" || tag == internalLabel || isInternalDownloadTag(tag) {
			continue
		}
		public = append(public, tag)
	}
	return strings.Join(public, ",")
}

func isInternalDownloadTag(tag string) bool {
	if len(tag) != len("mp-")+32 || !strings.HasPrefix(tag, "mp-") {
		return false
	}
	for _, character := range tag[len("mp-"):] {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func (d Dependencies) downloadPaths(w http.ResponseWriter, r *http.Request) {
	directories, err := d.loadDownloadDirectories(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取下载目录失败")
		return
	}

	paths := make([]downloadDirectoryView, 0, len(directories))
	for _, directory := range directories {
		path, ok := localDownloadPath(directory)
		if !ok {
			continue
		}
		priority := 0
		if directory.Priority != nil {
			priority = *directory.Priority
		}
		paths = append(paths, downloadDirectoryView{
			Name:          directory.Name,
			Storage:       "local",
			DownloadPath:  path,
			SavePath:      path,
			Priority:      priority,
			MediaType:     directory.MediaType,
			MediaCategory: directory.MediaCategory,
		})
	}
	writeJSON(w, http.StatusOK, paths)
}

func (d Dependencies) submitDownload(w http.ResponseWriter, r *http.Request) {
	request, err := decodeDownloadRequest(r, d.Config.MaxRequestBody)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "下载请求 JSON 无效")
		return
	}

	claims, ok := claimsFromContext(r.Context())
	username := strings.TrimSpace(claims.Username)
	if !ok || username == "" {
		writeDownloadFailure(w, "用户身份无效")
		return
	}

	siteID, locator, err := parseDownloadEnclosure(request.TorrentIn.Enclosure)
	if err != nil || request.TorrentIn.Site <= 0 || request.TorrentIn.Site != siteID {
		writeDownloadFailure(w, "下载来源无效")
		return
	}
	resolver, err := d.downloadResolver(siteID)
	if err != nil {
		writeDownloadFailure(w, "下载来源无效")
		return
	}

	selectedDownloader, err := d.selectDownloadClient(
		r.Context(),
		request.Downloader,
		request.TorrentIn.SiteDownloader,
	)
	if err != nil {
		if errors.Is(err, errDownloadRequestStoreUnavailable) {
			writeDownloadFailure(w, "下载服务暂不可用")
		} else {
			writeDownloadFailure(w, "没有可用的下载器")
		}
		return
	}

	savePath, err := d.allowedDownloadSavePath(r.Context(), request.SavePath)
	if err != nil {
		if errors.Is(err, errDownloadRequestStoreUnavailable) {
			writeDownloadFailure(w, "下载服务暂不可用")
		} else {
			writeDownloadFailure(w, "保存路径不在允许的下载目录中")
		}
		return
	}

	resourceID, resourceSecret, err := downloadflow.ProtectResourceLocator(locator)
	if err != nil {
		writeDownloadFailure(w, "下载来源无效")
		return
	}
	resolverKind := strings.ToLower(strings.TrimSpace(resolver.ResolverKind()))
	resolverVersion := resolver.ResolverVersion()
	reference := downloadflow.SourceReference{
		SiteID:          int64(siteID),
		ResolverKind:    resolverKind,
		ResolverVersion: resolverVersion,
		ResourceID:      resourceID,
		DownloaderID:    selectedDownloader.ID,
	}
	candidate, err := reference.Candidate(savePath, false)
	if err != nil {
		writeDownloadFailure(w, "下载请求无效")
		return
	}
	intent, err := downloader.NewIntent(username, candidate)
	if err != nil {
		writeDownloadFailure(w, "下载请求无效")
		return
	}
	plan := database.DownloadIntentPlan{
		IntentID:        intent.ID,
		SiteID:          int64(siteID),
		DownloaderID:    selectedDownloader.ID,
		ResolverKind:    resolverKind,
		ResolverVersion: resolverVersion,
		ResourceID:      resourceID,
		ResourceSecret:  resourceSecret,
		SavePath:        savePath,
		Paused:          false,
	}

	if d.DownloadSubmissions == nil {
		writeDownloadFailure(w, "下载服务暂不可用")
		return
	}
	successInput, err := buildDownloadSuccessInput(intent.ID, username, request.TorrentIn, request.MediaIn)
	if err != nil {
		writeDownloadFailure(w, "下载请求无效")
		return
	}
	downloadID, err := d.DownloadSubmissions.Submit(r.Context(), username, intent, plan, successInput)
	if err != nil || downloadID == uuid.Nil {
		writeDownloadFailure(w, "任务添加失败")
		return
	}
	writeSuccess(w, map[string]any{"download_id": downloadID.String()})
}

func buildDownloadSuccessInput(
	intentID uuid.UUID,
	username string,
	torrent downloadTorrentInput,
	mediaInput downloadMediaInput,
) (organization.SuccessInput, error) {
	parsed := media.Parse(strings.TrimSpace(torrent.Title+" "+torrent.Description), media.Options{})
	episodeNumbers, err := parsed.EpisodeList()
	if err != nil {
		return organization.SuccessInput{}, err
	}
	season := parsed.BeginSeason
	if season <= 0 && len(episodeNumbers) > 0 {
		if mediaInput.Season != nil {
			season = *mediaInput.Season
		} else {
			season = 1
		}
	}
	episodes := make([]organization.Episode, 0, len(episodeNumbers))
	for _, episode := range episodeNumbers {
		episodes = append(episodes, organization.Episode{Season: season, Episode: episode})
	}

	source, mediaID := downloadMediaIdentity(mediaInput)
	input := organization.SuccessInput{
		IntentID:   intentID,
		Version:    1,
		Username:   username,
		Manual:     false,
		Background: true,
		Media: organization.MediaContext{
			Source:  source,
			MediaID: mediaID,
			// V2 的 MediaInfo.from_dict 会跳过 title_year 属性，必须由 title 和 year 重算。
			TitleYear:   downloadMediaTitleYear(mediaInput.Title, mediaInput.Year),
			MediaType:   mediaInput.MediaType,
			Category:    mediaInput.Category,
			VoteAverage: mediaInput.VoteAverage,
			PosterURL:   mediaInput.PosterPath,
			BackdropURL: mediaInput.BackdropPath,
		},
		ResourceTerm: strings.Join(nonEmptyStrings(
			parsed.ResourceType,
			parsed.ResourceEffect,
			parsed.Resolution,
		), " "),
		Episodes: episodes,
	}
	return organization.NormalizeSuccessInput(input)
}

func downloadMediaIdentity(input downloadMediaInput) (string, string) {
	source := strings.ToLower(strings.TrimSpace(input.Source))
	if source == "tmdb" {
		source = "themoviedb"
	}
	if mediaID := strings.TrimSpace(input.MediaID); source != "" && mediaID != "" {
		return source, mediaID
	}

	identities := []struct {
		source string
		id     string
	}{
		{source: "themoviedb", id: positiveDownloadMediaID(input.TMDBID)},
		{source: "douban", id: strings.TrimSpace(input.DoubanID)},
		{source: "bangumi", id: positiveDownloadMediaID(input.BangumiID)},
		{source: "anilist", id: positiveDownloadMediaID(input.AniListID)},
	}
	if source != "" {
		for _, identity := range identities {
			if identity.source == source && identity.id != "" {
				return identity.source, identity.id
			}
		}
		return "", ""
	}
	for _, identity := range identities {
		if identity.id != "" {
			return identity.source, identity.id
		}
	}
	return "", ""
}

func positiveDownloadMediaID(id int64) string {
	if id <= 0 {
		return ""
	}
	return strconv.FormatInt(id, 10)
}

func downloadMediaTitleYear(title, year string) string {
	title = strings.TrimSpace(title)
	year = strings.TrimSpace(year)
	if title == "" || year == "" {
		return title
	}
	return title + " (" + year + ")"
}

func nonEmptyStrings(values ...string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			result = append(result, value)
		}
	}
	return result
}

var (
	errDownloadRequestStoreUnavailable = errors.New("下载请求设置存储不可用")
	errDownloadClientUnavailable       = errors.New("下载器不可用")
	errDownloadPathNotAllowed          = errors.New("下载路径不在允许范围")
	errDownloadResolverUnavailable     = errors.New("下载来源解析器不可用")
)

func decodeDownloadRequest(r *http.Request, maxBytes int64) (downloadRequest, error) {
	if r == nil || r.Body == nil {
		return downloadRequest{}, errors.New("下载请求为空")
	}
	if maxBytes <= 0 {
		maxBytes = defaultDownloadRequestBodyLimit
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBytes+1))
	if err != nil || int64(len(body)) > maxBytes {
		return downloadRequest{}, errors.New("读取下载请求失败")
	}

	decoder := json.NewDecoder(bytes.NewReader(body))
	var request downloadRequest
	if err := decoder.Decode(&request); err != nil {
		return downloadRequest{}, errors.New("下载请求 JSON 无效")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return downloadRequest{}, errors.New("下载请求包含多个 JSON 值")
	}
	return request, nil
}

func parseDownloadEnclosure(value string) (int, string, error) {
	if value == "" || !strings.HasPrefix(value, "/api/v1/resource/download/") {
		return 0, "", errors.New("资源地址无效")
	}
	parsed, err := url.ParseRequestURI(value)
	if err != nil || parsed.IsAbs() || parsed.Host != "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return 0, "", errors.New("资源地址无效")
	}
	parts := strings.Split(parsed.EscapedPath(), "/")
	if len(parts) != 7 || parts[0] != "" || parts[1] != "api" || parts[2] != "v1" ||
		parts[3] != "resource" || parts[4] != "download" || parts[5] == "" || parts[6] == "" {
		return 0, "", errors.New("资源地址无效")
	}
	siteID, err := strconv.Atoi(parts[5])
	if err != nil || siteID <= 0 {
		return 0, "", errors.New("资源地址无效")
	}
	locator, err := url.PathUnescape(parts[6])
	if err != nil || strings.TrimSpace(locator) == "" || locator == "." || locator == ".." || strings.ContainsRune(locator, 0) {
		return 0, "", errors.New("资源地址无效")
	}
	return siteID, locator, nil
}

func (d Dependencies) downloadResolver(siteID int) (indexer.VersionedDownloadResolver, error) {
	if d.SearchRegistry == nil || siteID <= 0 {
		return nil, errDownloadResolverUnavailable
	}
	for _, adapter := range d.SearchRegistry.Snapshot() {
		identified, ok := adapter.(indexer.SiteIDAdapter)
		if !ok || identified.SiteID() != siteID {
			continue
		}
		resolver, ok := adapter.(indexer.VersionedDownloadResolver)
		if !ok || strings.TrimSpace(resolver.ResolverKind()) == "" || resolver.ResolverVersion() <= 0 {
			continue
		}
		return resolver, nil
	}
	return nil, errDownloadResolverUnavailable
}

func (d Dependencies) selectDownloadClient(
	ctx context.Context,
	requestedName string,
	siteDownloader string,
) (downloadconfig.Downloader, error) {
	store := d.downloadRequestStore()
	if store == nil {
		return downloadconfig.Downloader{}, errDownloadRequestStoreUnavailable
	}
	downloaders, found, err := store.LoadDownloaders(ctx)
	if err != nil {
		return downloadconfig.Downloader{}, errDownloadRequestStoreUnavailable
	}
	if !found {
		return downloadconfig.Downloader{}, errDownloadClientUnavailable
	}

	selector := strings.TrimSpace(requestedName)
	if selector == "" {
		selector = strings.TrimSpace(siteDownloader)
	}
	if selector != "" {
		if selected, ok := downloadconfig.SelectEnabledDownloader(downloaders, selector); ok {
			return selected, nil
		}
		return downloadconfig.Downloader{}, errDownloadClientUnavailable
	}
	if selected, ok := downloadconfig.SelectEnabledDownloader(downloaders, ""); ok {
		return selected, nil
	}
	return downloadconfig.Downloader{}, errDownloadClientUnavailable
}

func usableQBittorrent(configured downloadconfig.Downloader) bool {
	return downloadconfig.UsableDownloader(configured)
}

func (d Dependencies) allowedDownloadSavePath(ctx context.Context, requested string) (string, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return "", nil
	}
	directories, err := d.loadDownloadDirectories(ctx)
	if err != nil {
		return "", errDownloadRequestStoreUnavailable
	}
	if path, ok := downloadconfig.AllowedLocalSavePath(directories, requested); ok {
		return path, nil
	}
	return "", errDownloadPathNotAllowed
}

func (d Dependencies) loadDownloadDirectories(ctx context.Context) ([]downloadDirectorySetting, error) {
	store := d.downloadRequestStore()
	if store == nil {
		return nil, errDownloadRequestStoreUnavailable
	}
	var directories []downloadDirectorySetting
	found, err := store.GetSetting(ctx, directoriesSettingKey, &directories)
	if err != nil {
		return nil, errDownloadRequestStoreUnavailable
	}
	if !found || directories == nil {
		return []downloadDirectorySetting{}, nil
	}
	return directories, nil
}

func (d Dependencies) downloadRequestStore() DownloadRequestStore {
	if d.DownloadRequests != nil {
		return d.DownloadRequests
	}
	if d.Store != nil {
		return d.Store
	}
	return nil
}

func localDownloadPath(directory downloadDirectorySetting) (string, bool) {
	return directory.LocalDownloadPath()
}

func writeDownloadFailure(w http.ResponseWriter, message string) {
	writeError(w, http.StatusOK, message)
}
