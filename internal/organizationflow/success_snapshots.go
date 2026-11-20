package organizationflow

import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/media"
	"github.com/moviepilot-go/moviepilot-go/internal/organization"
	"github.com/moviepilot-go/moviepilot-go/internal/organizer"
)

const (
	organizationFileRoleVideo    = "video"
	organizationFileRoleSubtitle = "subtitle"
	organizationFileRoleAudio    = "audio"
	organizationFileRoleOther    = "other"
)

var (
	organizationVideoExtensions = extensionSet(
		".mp4", ".mkv", ".ts", ".iso", ".rmvb", ".avi", ".mov", ".mpeg", ".mpg",
		".wmv", ".3gp", ".asf", ".m4v", ".flv", ".m2ts", ".strm", ".tp", ".f4v",
	)
	organizationSubtitleExtensions = extensionSet(".srt", ".ass", ".ssa", ".sup")
	organizationAudioExtensions    = extensionSet(
		".aac", ".ac3", ".amr", ".caf", ".cda", ".dsf", ".dff", ".kar", ".m4a",
		".mp1", ".mp2", ".mp3", ".mid", ".mod", ".mka", ".mpc", ".nsf", ".ogg",
		".pcm", ".rmi", ".s3m", ".snd", ".spx", ".tak", ".tta", ".vqf", ".wav",
		".wma", ".aifc", ".aiff", ".alac", ".adif", ".adts", ".flac", ".midi",
	)
)

type preparedSuccessFile struct {
	fact organization.FileFact
	stem string
}

func buildSuccessSnapshots(
	input organization.SuccessInput,
	needNotify bool,
	plan organizer.Plan,
) ([]organization.SuccessSnapshot, error) {
	if input.IntentID == uuid.Nil || plan.ID == "" || len(plan.Operations) == 0 {
		return nil, ErrInvalidOrganizationContent
	}
	batchID := uuid.NewSHA1(input.IntentID, []byte(plan.ID))
	files, err := buildSuccessFileFacts(input, plan)
	if err != nil {
		return nil, err
	}

	groups := make(map[int][]organization.FileFact)
	for _, file := range files {
		groups[file.fact.Season] = append(groups[file.fact.Season], file.fact.Clone())
	}
	seasons := make([]int, 0, len(groups))
	for season := range groups {
		seasons = append(seasons, season)
	}
	sort.Ints(seasons)

	snapshots := make([]organization.SuccessSnapshot, 0, len(seasons))
	for _, season := range seasons {
		groupKey, err := organization.BuildGroupKey(input.Media, season)
		if err != nil {
			return nil, ErrInvalidOrganizationContent
		}
		snapshot, err := organization.NormalizeSuccessSnapshot(organization.SuccessSnapshot{
			BatchID: batchID, IntentID: input.IntentID, GroupKey: groupKey,
			Version: input.Version, Legacy: input.Legacy, Username: input.Username,
			NeedNotify: needNotify, Manual: input.Manual, Background: input.Background,
			Media: input.Media.Clone(), Files: groups[season],
		})
		if err != nil {
			return nil, ErrInvalidOrganizationContent
		}
		snapshots = append(snapshots, snapshot)
	}
	sort.Slice(snapshots, func(i, j int) bool { return snapshots[i].GroupKey < snapshots[j].GroupKey })
	return snapshots, nil
}

func buildSuccessFileFacts(
	input organization.SuccessInput,
	plan organizer.Plan,
) ([]preparedSuccessFile, error) {
	files := make([]preparedSuccessFile, len(plan.Operations))
	defaultSeason := successInputDefaultSeason(input)
	for ordinal, operation := range plan.Operations {
		role := organizationFileRole(operation.Source)
		season := 0
		var episodes []int
		if !input.Legacy && input.Media.MediaType == string(media.TypeTV) {
			parsed := media.Parse(filepath.Base(operation.Source), media.Options{})
			season = parsed.BeginSeason
			if season <= 0 {
				season = defaultSeason
			}
			var err error
			episodes, err = parsed.EpisodeList()
			if err != nil {
				return nil, ErrInvalidOrganizationContent
			}
		}
		files[ordinal] = preparedSuccessFile{
			fact: organization.FileFact{
				Ordinal: ordinal, Role: role, SizeBytes: operation.ExpectedSize,
				Season: season, Episodes: episodes, ResourceTerm: input.ResourceTerm,
			},
			stem: successFileStem(operation.Source),
		}
	}
	if input.Legacy || input.Media.MediaType != string(media.TypeTV) {
		return files, nil
	}
	assignUnparsedVideoEpisodes(files, input.Episodes)
	assignRelatedFileEpisodes(files)
	return files, nil
}

func assignUnparsedVideoEpisodes(files []preparedSuccessFile, episodes []organization.Episode) {
	remaining := make(map[int][]int)
	seen := make(map[organization.Episode]struct{})
	for _, file := range files {
		for _, episode := range file.fact.Episodes {
			seen[organization.Episode{Season: file.fact.Season, Episode: episode}] = struct{}{}
		}
	}
	for _, episode := range episodes {
		if _, exists := seen[episode]; !exists {
			remaining[episode.Season] = append(remaining[episode.Season], episode.Episode)
		}
	}

	unresolved := make(map[int][]int)
	for index := range files {
		if files[index].fact.Role == organizationFileRoleVideo && len(files[index].fact.Episodes) == 0 {
			unresolved[files[index].fact.Season] = append(unresolved[files[index].fact.Season], index)
		}
	}
	for season, indexes := range unresolved {
		available := remaining[season]
		switch {
		case len(indexes) == 1:
			files[indexes[0]].fact.Episodes = append([]int(nil), available...)
		case len(indexes) == len(available):
			for offset, index := range indexes {
				files[index].fact.Episodes = []int{available[offset]}
			}
		}
	}
}

func assignRelatedFileEpisodes(files []preparedSuccessFile) {
	for index := range files {
		if files[index].fact.Role == organizationFileRoleVideo || len(files[index].fact.Episodes) != 0 {
			continue
		}
		best := -1
		for candidate := range files {
			if files[candidate].fact.Role != organizationFileRoleVideo ||
				files[candidate].fact.Season != files[index].fact.Season ||
				!successFileStemsRelated(files[candidate].stem, files[index].stem) {
				continue
			}
			if best == -1 || len(files[candidate].stem) > len(files[best].stem) {
				best = candidate
			}
		}
		if best >= 0 {
			files[index].fact.Episodes = append([]int(nil), files[best].fact.Episodes...)
		}
	}
}

func successInputDefaultSeason(input organization.SuccessInput) int {
	season := -1
	for _, episode := range input.Episodes {
		if season == -1 {
			season = episode.Season
			continue
		}
		if season != episode.Season {
			return 1
		}
	}
	if season >= 0 {
		return season
	}
	return 1
}

func organizationFileRole(source string) string {
	extension := strings.ToLower(filepath.Ext(source))
	if _, ok := organizationVideoExtensions[extension]; ok {
		return organizationFileRoleVideo
	}
	if _, ok := organizationSubtitleExtensions[extension]; ok {
		return organizationFileRoleSubtitle
	}
	if _, ok := organizationAudioExtensions[extension]; ok {
		return organizationFileRoleAudio
	}
	return organizationFileRoleOther
}

func successFileStem(source string) string {
	base := filepath.Base(source)
	return strings.ToLower(strings.TrimSuffix(base, filepath.Ext(base)))
}

func successFileStemsRelated(videoStem, extraStem string) bool {
	return extraStem == videoStem || strings.HasPrefix(extraStem, videoStem+".") ||
		strings.HasPrefix(extraStem, videoStem+"-") || strings.HasPrefix(extraStem, videoStem+"_")
}

func extensionSet(values ...string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		result[value] = struct{}{}
	}
	return result
}
