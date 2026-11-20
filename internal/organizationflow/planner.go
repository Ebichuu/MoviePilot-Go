package organizationflow

import (
	"context"
	"errors"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/moviepilot-go/moviepilot-go/internal/downloadconfig"
	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
	"github.com/moviepilot-go/moviepilot-go/internal/organization"
	"github.com/moviepilot-go/moviepilot-go/internal/organizer"
)

const (
	maxPlannedFiles = 10_000
	maxPlanPathLen  = 4096
)

var (
	// ErrInvalidOrganizationContent indicates a malformed or unsafe local path.
	ErrInvalidOrganizationContent = errors.New("整理内容路径无效")
	// ErrOrganizationContentUnavailable indicates that the completed content is
	// not currently available as the exact regular files reported by downloader.
	ErrOrganizationContentUnavailable = errors.New("整理内容当前不可用")
	// ErrOrganizationRuleUnavailable indicates that no enabled local V2
	// directory maps the completed download to a media-library target.
	ErrOrganizationRuleUnavailable = errors.New("未找到可用的本地整理目录")
	// ErrOrganizationPlanLimit indicates that one download exceeds the bounded
	// number of files accepted by the first local organizer implementation.
	ErrOrganizationPlanLimit = errors.New("整理计划文件数超过上限")
)

// PreparedPlan is the immutable file plan plus the source boundary persisted
// with an organization batch.
type PreparedPlan struct {
	SourceRoot string
	Plan       organizer.Plan
	Snapshots  []organization.SuccessSnapshot
}

type localRule struct {
	downloadRoot          string
	libraryRoot           string
	mode                  organizer.Mode
	priority              int
	needNotify            bool
	renaming              bool
	libraryTypeFolder     bool
	libraryCategoryFolder bool
}

// BuildPlan maps the downloader's exact selected and completed file snapshot
// through the most specific matching V2 Directories rule. It never walks the
// download directory, so unselected or unrelated files cannot enter the plan.
func BuildPlan(
	ctx context.Context,
	content downloader.TaskContent,
	directories []downloadconfig.Directory,
	successInput organization.SuccessInput,
) (PreparedPlan, error) {
	return BuildPlanWithTemplates(ctx, content, directories, successInput, organization.NamingTemplates{})
}

// BuildPlanWithTemplates 在 BuildPlan 基础上支持 V2 自定义 Jinja2 命名模板：
// 当目录规则启用 renaming 或配置了自定义模板时，对视频文件按模板渲染目标相对路径，
// 并按需拼接二级类型/分类目录；模板渲染失败时自动回退到原始相对路径。
func BuildPlanWithTemplates(
	ctx context.Context,
	content downloader.TaskContent,
	directories []downloadconfig.Directory,
	successInput organization.SuccessInput,
	templates organization.NamingTemplates,
) (PreparedPlan, error) {
	if ctx == nil {
		return PreparedPlan{}, ErrInvalidOrganizationContent
	}
	if err := ctx.Err(); err != nil {
		return PreparedPlan{}, err
	}
	if content.Task.State != downloader.TaskStateCompleted || len(content.Files) == 0 {
		return PreparedPlan{}, ErrOrganizationContentUnavailable
	}
	if len(content.Files) > maxPlannedFiles {
		return PreparedPlan{}, ErrOrganizationPlanLimit
	}
	normalizedInput, err := organization.NormalizeSuccessInput(successInput)
	if err != nil {
		return PreparedPlan{}, ErrInvalidOrganizationContent
	}

	saveRoot, err := normalizedAbsolutePath(content.Task.SavePath)
	if err != nil {
		return PreparedPlan{}, err
	}
	contentRoot, err := normalizedAbsolutePath(content.Task.ContentPath)
	if err != nil {
		return PreparedPlan{}, err
	}
	contentInfo, resolvedContentRoot, err := inspectContentRoot(ctx, contentRoot)
	if err != nil {
		return PreparedPlan{}, err
	}

	rule, ok := selectLocalRule(contentRoot, directories)
	if !ok {
		return PreparedPlan{}, ErrOrganizationRuleUnavailable
	}
	if pathsOverlap(contentRoot, rule.libraryRoot) {
		return PreparedPlan{}, ErrInvalidOrganizationContent
	}

	operations, err := buildOperations(
		ctx,
		saveRoot,
		contentRoot,
		resolvedContentRoot,
		contentInfo.IsDir(),
		content.Files,
		rule,
		normalizedInput,
		templates,
	)
	if err != nil {
		return PreparedPlan{}, err
	}
	plan, err := organizer.BuildPlan(rule.libraryRoot, operations)
	if err != nil {
		return PreparedPlan{}, ErrInvalidOrganizationContent
	}
	snapshots, err := buildSuccessSnapshots(normalizedInput, rule.needNotify, plan)
	if err != nil {
		return PreparedPlan{}, err
	}
	return PreparedPlan{SourceRoot: contentRoot, Plan: plan, Snapshots: snapshots}, nil
}

func inspectContentRoot(ctx context.Context, contentRoot string) (os.FileInfo, string, error) {
	info, err := inspectOrganizationPath(ctx, contentRoot)
	if err != nil {
		return nil, "", err
	}
	if !info.Mode().IsRegular() && !info.IsDir() {
		return nil, "", ErrOrganizationContentUnavailable
	}
	return info, contentRoot, nil
}

func inspectOrganizationPath(ctx context.Context, candidate string) (os.FileInfo, error) {
	ancestors := make([]string, 0, 16)
	for current := filepath.Clean(candidate); ; current = filepath.Dir(current) {
		ancestors = append(ancestors, current)
		next := filepath.Dir(current)
		if samePath(next, current) {
			break
		}
	}
	for left, right := 0, len(ancestors)-1; left < right; left, right = left+1, right-1 {
		ancestors[left], ancestors[right] = ancestors[right], ancestors[left]
	}

	var candidateInfo os.FileInfo
	for index, current := range ancestors {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		info, err := os.Lstat(current)
		if err != nil {
			if contextErr := ctx.Err(); contextErr != nil {
				return nil, contextErr
			}
			return nil, ErrOrganizationContentUnavailable
		}
		if isUnsafeOrganizationPathLink(info) || (index < len(ancestors)-1 && !info.IsDir()) {
			return nil, ErrOrganizationContentUnavailable
		}
		candidateInfo = info
	}
	return candidateInfo, nil
}

func selectLocalRule(contentPath string, directories []downloadconfig.Directory) (localRule, bool) {
	rules := make([]localRule, 0, len(directories))
	for _, directory := range directories {
		downloadRoot, downloadOK := directory.LocalDownloadPath()
		libraryRoot, libraryOK := directory.LocalLibraryPath()
		mode, modeOK := organizationMode(directory.TransferType)
		if !downloadOK || !libraryOK || !modeOK {
			continue
		}
		downloadRoot, err := normalizedAbsolutePath(downloadRoot)
		if err != nil || !pathWithinRoot(downloadRoot, contentPath) {
			continue
		}
		libraryRoot, err = normalizedAbsolutePath(libraryRoot)
		if err != nil || samePath(downloadRoot, libraryRoot) {
			continue
		}
		priority := 0
		if directory.Priority != nil {
			priority = *directory.Priority
		}
		needNotify := true
		if directory.Notify != nil {
			needNotify = *directory.Notify
		}
		renaming := directory.Renaming != nil && *directory.Renaming
		libraryTypeFolder := directory.LibraryTypeFolder != nil && *directory.LibraryTypeFolder
		libraryCategoryFolder := directory.LibraryCategoryFolder != nil && *directory.LibraryCategoryFolder
		rules = append(rules, localRule{
			downloadRoot:          downloadRoot,
			libraryRoot:           libraryRoot,
			mode:                  mode,
			priority:              priority,
			needNotify:            needNotify,
			renaming:              renaming,
			libraryTypeFolder:     libraryTypeFolder,
			libraryCategoryFolder: libraryCategoryFolder,
		})
	}
	if len(rules) == 0 {
		return localRule{}, false
	}
	sort.SliceStable(rules, func(i, j int) bool {
		leftDepth := pathDepth(rules[i].downloadRoot)
		rightDepth := pathDepth(rules[j].downloadRoot)
		if leftDepth != rightDepth {
			return leftDepth > rightDepth
		}
		if rules[i].priority != rules[j].priority {
			return rules[i].priority < rules[j].priority
		}
		return rules[i].downloadRoot < rules[j].downloadRoot
	})
	return rules[0], true
}

func buildOperations(
	ctx context.Context,
	saveRoot string,
	contentRoot string,
	resolvedContentRoot string,
	contentIsDirectory bool,
	files []downloader.TaskContentFile,
	rule localRule,
	successInput organization.SuccessInput,
	templates organization.NamingTemplates,
) ([]organizer.Operation, error) {
	type plannedFile struct {
		relative string
		size     int64
	}
	planned := make([]plannedFile, 0, len(files))
	seenRelative := make(map[string]struct{}, len(files))
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		relative, err := normalizedSnapshotRelativePath(file.RelativePath)
		if err != nil || file.SizeBytes < 0 {
			return nil, ErrInvalidOrganizationContent
		}
		key := comparablePath(relative)
		if _, exists := seenRelative[key]; exists {
			return nil, ErrInvalidOrganizationContent
		}
		seenRelative[key] = struct{}{}
		planned = append(planned, plannedFile{relative: relative, size: file.SizeBytes})
	}
	sort.Slice(planned, func(i, j int) bool {
		return comparablePath(planned[i].relative) < comparablePath(planned[j].relative)
	})

	operations := make([]organizer.Operation, 0, len(planned))
	seenTargets := make(map[string]struct{}, len(planned))
	for _, file := range planned {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		source := filepath.Clean(filepath.Join(saveRoot, filepath.FromSlash(file.relative)))
		if len(source) > maxPlanPathLen || !pathWithinRoot(saveRoot, source) || !pathWithinRoot(contentRoot, source) {
			return nil, ErrInvalidOrganizationContent
		}
		if !contentIsDirectory && !samePath(source, contentRoot) {
			return nil, ErrInvalidOrganizationContent
		}
		info, err := inspectOrganizationPath(ctx, source)
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() || info.Size() != file.size {
			return nil, ErrOrganizationContentUnavailable
		}
		expectedSize, expectedSHA256, err := organizer.FingerprintFile(ctx, source)
		if err != nil {
			if contextErr := ctx.Err(); contextErr != nil {
				return nil, contextErr
			}
			return nil, ErrOrganizationContentUnavailable
		}
		if expectedSize != file.size {
			return nil, ErrOrganizationContentUnavailable
		}
		if !pathWithinRoot(resolvedContentRoot, source) {
			return nil, ErrInvalidOrganizationContent
		}

		targetRelative := filepath.Base(contentRoot)
		if contentIsDirectory {
			relativeToContent, err := filepath.Rel(contentRoot, source)
			if err != nil || filepath.IsAbs(relativeToContent) || relativeEscapes(relativeToContent) || relativeToContent == "." {
				return nil, ErrInvalidOrganizationContent
			}
			targetRelative = filepath.Join(targetRelative, relativeToContent)
		}
		hasCustomTemplate := strings.TrimSpace(templates.Movie) != "" || strings.TrimSpace(templates.TV) != ""
		if (rule.renaming || hasCustomTemplate) && organization.IsVideoFilename(source) {
			if rendered, renderErr := renderPlannedTargetRelative(source, successInput, rule, templates); renderErr == nil && rendered != "" {
				targetRelative = filepath.FromSlash(rendered)
			}
		}
		target := filepath.Clean(filepath.Join(rule.libraryRoot, targetRelative))
		if len(target) > maxPlanPathLen || !pathWithinRoot(rule.libraryRoot, target) {
			return nil, ErrInvalidOrganizationContent
		}
		targetKey := comparablePath(target)
		if _, exists := seenTargets[targetKey]; exists {
			return nil, ErrInvalidOrganizationContent
		}
		seenTargets[targetKey] = struct{}{}
		operations = append(operations, organizer.Operation{
			Source:         source,
			Target:         target,
			Mode:           rule.mode,
			ExpectedSize:   expectedSize,
			ExpectedSHA256: expectedSHA256,
		})
	}
	return operations, nil
}

func normalizedSnapshotRelativePath(value string) (string, error) {
	if value == "" || len(value) > maxPlanPathLen || strings.IndexByte(value, 0) >= 0 {
		return "", ErrInvalidOrganizationContent
	}
	value = strings.ReplaceAll(value, `\`, "/")
	if strings.HasPrefix(value, "/") || hasWindowsDrivePrefix(value) {
		return "", ErrInvalidOrganizationContent
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == ".." {
			return "", ErrInvalidOrganizationContent
		}
	}
	value = path.Clean(value)
	if value == "." || value == ".." || strings.HasPrefix(value, "../") || path.IsAbs(value) ||
		hasWindowsDrivePrefix(value) {
		return "", ErrInvalidOrganizationContent
	}
	return value, nil
}

func hasWindowsDrivePrefix(value string) bool {
	// filepath on Windows treats every single-character volume prefix as a
	// rooted path (for example "0:/file"), not only alphabetic drive letters.
	// Reject the complete platform grammar so a snapshot accepted on Linux
	// cannot become absolute when the persisted plan later runs on Windows.
	return len(value) >= 2 && value[1] == ':'
}

func organizationMode(value *string) (organizer.Mode, bool) {
	if value == nil {
		return "", false
	}
	switch strings.ToLower(strings.TrimSpace(*value)) {
	case "copy":
		return organizer.Copy, true
	case "move":
		return organizer.Move, true
	case "link", "hardlink":
		return organizer.Hardlink, true
	case "softlink", "symlink":
		return organizer.Symlink, true
	default:
		return "", false
	}
}

func normalizedAbsolutePath(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > maxPlanPathLen || strings.IndexByte(value, 0) >= 0 {
		return "", ErrInvalidOrganizationContent
	}
	abs, err := filepath.Abs(value)
	if err != nil || len(abs) > maxPlanPathLen {
		return "", ErrInvalidOrganizationContent
	}
	return filepath.Clean(abs), nil
}

func pathWithinRoot(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && !filepath.IsAbs(relative) && !relativeEscapes(relative)
}

func pathsOverlap(left, right string) bool {
	return pathWithinRoot(left, right) || pathWithinRoot(right, left)
}

func relativeEscapes(relative string) bool {
	return relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func samePath(left, right string) bool {
	return comparablePath(left) == comparablePath(right)
}

func comparablePath(value string) string {
	value = filepath.Clean(value)
	if filepath.Separator == '\\' {
		return strings.ToLower(value)
	}
	return value
}

func pathDepth(value string) int {
	volume := filepath.VolumeName(value)
	trimmed := strings.TrimPrefix(value, volume)
	parts := strings.FieldsFunc(trimmed, func(r rune) bool {
		return r == '/' || r == '\\'
	})
	return len(parts)
}

func renderPlannedTargetRelative(
	source string,
	successInput organization.SuccessInput,
	rule localRule,
	templates organization.NamingTemplates,
) (string, error) {
	title, year := splitPlanTitleYear(successInput.Media.TitleYear)
	tmdbID := ""
	if strings.EqualFold(strings.TrimSpace(successInput.Media.Source), "themoviedb") {
		tmdbID = strings.TrimSpace(successInput.Media.MediaID)
	}
	rel, err := organization.RenderRelativeTarget(organization.NamingInput{
		SourcePath: source,
		MediaType:  successInput.Media.MediaType,
		Title:      title,
		Year:       year,
		Category:   successInput.Media.Category,
		TMDBID:     tmdbID,
	}, templates)
	if err != nil {
		return "", err
	}
	prefix := make([]string, 0, 2)
	if rule.libraryTypeFolder && strings.TrimSpace(successInput.Media.MediaType) != "" {
		prefix = append(prefix, strings.TrimSpace(successInput.Media.MediaType))
	}
	if rule.libraryCategoryFolder && strings.TrimSpace(successInput.Media.Category) != "" {
		prefix = append(prefix, strings.TrimSpace(successInput.Media.Category))
	}
	if len(prefix) > 0 {
		rel = path.Join(append(prefix, rel)...)
	}
	return rel, nil
}

func splitPlanTitleYear(value string) (string, string) {
	value = strings.TrimSpace(value)
	open := strings.LastIndex(value, "(")
	if open < 0 || !strings.HasSuffix(value, ")") {
		return value, ""
	}
	year := strings.TrimSpace(value[open+1 : len(value)-1])
	if len(year) != 4 {
		return value, ""
	}
	for _, r := range year {
		if r < '0' || r > '9' {
			return value, ""
		}
	}
	return strings.TrimSpace(value[:open]), year
}
