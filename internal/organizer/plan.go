package organizer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	maxPlanOperations = 10_000
	maxPlanPathLength = 4096
)

// Mode 是本地文件整理方式。
type Mode string

const (
	Copy     Mode = "copy"
	Move     Mode = "move"
	Hardlink Mode = "hardlink"
	Symlink  Mode = "symlink"
)

// Operation 是不可变整理计划中的单个文件操作。
type Operation struct {
	Source         string `json:"source"`
	Target         string `json:"target"`
	Mode           Mode   `json:"mode"`
	ExpectedSize   int64  `json:"expected_size"`
	ExpectedSHA256 string `json:"expected_sha256"`
}

// Plan 在执行副作用之前持久化，ID 由完整操作列表确定。
type Plan struct {
	ID         string      `json:"id"`
	TargetRoot string      `json:"target_root"`
	Operations []Operation `json:"operations"`
}

// BuildPlan 校验目标边界、文件指纹和有界输入，并生成确定性计划 ID。
func BuildPlan(targetRoot string, operations []Operation) (Plan, error) {
	root, err := normalizePlanPath(targetRoot)
	if err != nil {
		return Plan{}, errors.New("整理根目录无效")
	}
	if len(operations) == 0 || len(operations) > maxPlanOperations {
		return Plan{}, fmt.Errorf("整理计划文件数必须在 1 到 %d 之间", maxPlanOperations)
	}
	validated := make([]Operation, len(operations))
	seenTargets := make(map[string]struct{}, len(operations))
	for i, operation := range operations {
		normalized, normalizeErr := normalizeOperation(root, operation)
		if normalizeErr != nil {
			return Plan{}, fmt.Errorf("整理操作 %d 无效: %w", i, normalizeErr)
		}
		targetKey := comparablePlanPath(normalized.Target)
		if _, exists := seenTargets[targetKey]; exists {
			return Plan{}, fmt.Errorf("整理操作 %d 的目标路径重复", i)
		}
		seenTargets[targetKey] = struct{}{}
		validated[i] = normalized
	}
	encoded, err := json.Marshal(validated)
	if err != nil {
		return Plan{}, fmt.Errorf("编码整理计划: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return Plan{ID: hex.EncodeToString(digest[:16]), TargetRoot: root, Operations: validated}, nil
}

// Execute 执行整理计划。目标存在时仅接受指纹完全一致的幂等重试。
//
// Execute 保留早期调用方不提供下载根目录的兼容语义。生产整理任务应使用
// ExecuteWithinSourceRoot，以同时约束计划源文件的边界和路径祖先。
func Execute(ctx context.Context, plan Plan) error {
	return execute(ctx, plan, "", false)
}

// ExecuteWithinSourceRoot 在受信任的下载根目录内执行整理计划。
// 每次文件操作和 Move 源删除前都会重新验证源路径边界及路径祖先。
func ExecuteWithinSourceRoot(ctx context.Context, sourceRoot string, plan Plan) error {
	if ctx == nil {
		return errors.New("整理上下文无效")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	root, err := normalizePlanPath(sourceRoot)
	if err != nil {
		return errors.New("整理源根目录无效")
	}
	return execute(ctx, plan, root, true)
}

type executionBoundary struct {
	targetRoot        string
	sourceRoot        string
	enforceSourceRoot bool
}

func execute(ctx context.Context, plan Plan, sourceRoot string, enforceSourceRoot bool) error {
	if ctx == nil {
		return errors.New("整理上下文无效")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	root, err := normalizePlanPath(plan.TargetRoot)
	if err != nil || len(plan.Operations) == 0 || len(plan.Operations) > maxPlanOperations {
		return errors.New("整理计划无效")
	}
	validated := make([]Operation, len(plan.Operations))
	seenTargets := make(map[string]struct{}, len(plan.Operations))
	for index, operation := range plan.Operations {
		normalized, normalizeErr := normalizeOperation(root, operation)
		if normalizeErr != nil {
			return fmt.Errorf("整理操作 %d 无效: %w", index, normalizeErr)
		}
		targetKey := comparablePlanPath(normalized.Target)
		if _, exists := seenTargets[targetKey]; exists {
			return fmt.Errorf("整理操作 %d 的目标路径重复", index)
		}
		if enforceSourceRoot && !planPathWithinRoot(sourceRoot, normalized.Source) {
			return fmt.Errorf("整理操作 %d 的源路径越过下载根目录", index)
		}
		seenTargets[targetKey] = struct{}{}
		validated[index] = normalized
	}
	boundary := executionBoundary{
		targetRoot:        root,
		sourceRoot:        sourceRoot,
		enforceSourceRoot: enforceSourceRoot,
	}
	for index, operation := range validated {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := executeOperation(ctx, boundary, operation, plan.ID, index); err != nil {
			return fmt.Errorf("执行整理操作 %d: %w", index, err)
		}
	}
	return nil
}

func normalizeOperation(root string, operation Operation) (Operation, error) {
	if operation.Mode != Copy && operation.Mode != Move && operation.Mode != Hardlink && operation.Mode != Symlink {
		return Operation{}, fmt.Errorf("不支持的整理方式 %q", operation.Mode)
	}
	if operation.ExpectedSize < 0 || !validSHA256(operation.ExpectedSHA256) {
		return Operation{}, errors.New("文件指纹无效")
	}
	source, err := normalizePlanPath(operation.Source)
	if err != nil {
		return Operation{}, errors.New("源路径无效")
	}
	target, err := normalizePlanPath(operation.Target)
	if err != nil {
		return Operation{}, errors.New("目标路径无效")
	}
	if samePlanPath(source, target) {
		return Operation{}, errors.New("源路径和目标路径不能相同")
	}
	if !planPathWithinRoot(root, target) {
		return Operation{}, fmt.Errorf("目标路径越过整理根目录: %s", target)
	}
	return Operation{
		Source:         source,
		Target:         target,
		Mode:           operation.Mode,
		ExpectedSize:   operation.ExpectedSize,
		ExpectedSHA256: operation.ExpectedSHA256,
	}, nil
}

func normalizePlanPath(value string) (string, error) {
	if value == "" || len(value) > maxPlanPathLength || strings.IndexByte(value, 0) >= 0 {
		return "", errors.New("整理路径无效")
	}
	absolute, err := filepath.Abs(value)
	if err != nil || len(absolute) > maxPlanPathLength || strings.IndexByte(absolute, 0) >= 0 {
		return "", errors.New("整理路径无效")
	}
	return filepath.Clean(absolute), nil
}

func planPathWithinRoot(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func samePlanPath(left, right string) bool {
	relative, err := filepath.Rel(left, right)
	return err == nil && relative == "."
}

func validSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		if character < '0' || character > '9' {
			if character < 'a' || character > 'f' {
				return false
			}
		}
	}
	return true
}

func (boundary executionBoundary) validateSource(ctx context.Context, source string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !boundary.enforceSourceRoot {
		return nil
	}
	if err := validateSourceAncestors(ctx, boundary.sourceRoot, source); err != nil {
		return err
	}
	return verifyParentDirNoFollow(source)
}

func (boundary executionBoundary) validateTarget(ctx context.Context, target string) error {
	if err := validateTargetAncestors(ctx, boundary.targetRoot, target); err != nil {
		return err
	}
	return verifyParentDirNoFollow(target)
}

func executeOperation(
	ctx context.Context,
	boundary executionBoundary,
	operation Operation,
	planID string,
	index int,
) error {
	if err := boundary.validateSource(ctx, operation.Source); err != nil {
		return err
	}
	sourceInfo, err := os.Lstat(operation.Source)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && operation.Mode == Move {
			if ancestorErr := boundary.validateTarget(ctx, operation.Target); ancestorErr != nil {
				return ancestorErr
			}
			if targetErr := verifyPlannedTarget(ctx, operation); targetErr != nil {
				return fmt.Errorf("源文件不存在且目标指纹不匹配: %w", targetErr)
			}
			return nil
		}
		return fmt.Errorf("检查源文件: %w", err)
	}
	if isUnsafePathLink(sourceInfo) || !sourceInfo.Mode().IsRegular() {
		return errors.New("首版只整理普通文件")
	}
	if err := verifyFileFingerprintFromInfo(
		ctx, operation.Source, operation.ExpectedSize, operation.ExpectedSHA256, sourceInfo,
	); err != nil {
		return fmt.Errorf("源文件指纹不匹配: %w", err)
	}
	if err := boundary.validateSource(ctx, operation.Source); err != nil {
		return err
	}
	if err := boundary.validateTarget(ctx, operation.Target); err != nil {
		return err
	}

	if _, targetErr := os.Lstat(operation.Target); targetErr == nil {
		if err := verifyPlannedTarget(ctx, operation); err != nil {
			return fmt.Errorf("目标已存在且指纹不匹配: %w", err)
		}
		if operation.Mode == Move {
			return removeVerifiedMoveSource(ctx, boundary, operation)
		}
		return nil
	} else if !errors.Is(targetErr, os.ErrNotExist) {
		return fmt.Errorf("检查目标文件: %w", targetErr)
	}

	if err := ctx.Err(); err != nil {
		return err
	}
	if err := boundary.validateTarget(ctx, operation.Target); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(operation.Target), 0o755); err != nil {
		return fmt.Errorf("创建目标目录: %w", err)
	}
	if err := boundary.validateTarget(ctx, operation.Target); err != nil {
		return err
	}
	temporary := temporaryPath(operation.Target, planID, index)
	if err := boundary.validateTarget(ctx, operation.Target); err != nil {
		return err
	}
	if err := removeStaleTemporary(ctx, temporary); err != nil {
		return err
	}
	defer func() {
		_ = os.Remove(temporary)
	}()

	if err := boundary.validateTarget(ctx, operation.Target); err != nil {
		return err
	}
	if err := boundary.validateSource(ctx, operation.Source); err != nil {
		return err
	}
	switch operation.Mode {
	case Copy, Move:
		if err := copyFile(ctx, operation.Source, temporary, sourceInfo.Mode().Perm()); err != nil {
			return fmt.Errorf("复制临时文件: %w", err)
		}
	case Hardlink:
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := os.Link(operation.Source, temporary); err != nil {
			// 下载目录与媒体库位于不同挂载/文件系统时硬链接不可用（EXDEV）。
			// V2 的做法是降级为复制，否则整个整理批次会全部失败。
			if copyErr := copyFile(ctx, operation.Source, temporary, sourceInfo.Mode().Perm()); copyErr != nil {
				return fmt.Errorf("创建临时硬链接失败（%v），降级复制也失败: %w", err, copyErr)
			}
		}
	case Symlink:
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := os.Symlink(operation.Source, temporary); err != nil {
			return fmt.Errorf("创建临时软链接: %w", err)
		}
	}
	if err := boundary.validateTarget(ctx, operation.Target); err != nil {
		return err
	}
	if err := verifyPlannedPath(ctx, temporary, operation); err != nil {
		return fmt.Errorf("临时目标指纹不匹配: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := boundary.validateTarget(ctx, operation.Target); err != nil {
		return err
	}
	if err := publishNoReplace(temporary, operation.Target); err != nil {
		if ancestorErr := boundary.validateTarget(ctx, operation.Target); ancestorErr != nil {
			return ancestorErr
		}
		if targetErr := verifyPlannedTarget(ctx, operation); targetErr != nil {
			return fmt.Errorf("提交目标文件: %w", err)
		}
		if operation.Mode == Move {
			return removeVerifiedMoveSource(ctx, boundary, operation)
		}
		return nil
	}
	if err := boundary.validateTarget(ctx, operation.Target); err != nil {
		return err
	}
	if err := verifyPlannedTarget(ctx, operation); err != nil {
		return fmt.Errorf("最终目标指纹不匹配: %w", err)
	}
	if operation.Mode == Move {
		return removeVerifiedMoveSource(ctx, boundary, operation)
	}
	return nil
}

func validateTargetAncestors(ctx context.Context, targetRoot, target string) error {
	parent := filepath.Dir(target)
	if !planPathWithinRoot(targetRoot, parent) {
		return errors.New("目标父目录越过整理根目录")
	}
	return validateExistingDirectoryAncestors(ctx, parent, "目标")
}

func validateSourceAncestors(ctx context.Context, sourceRoot, source string) error {
	if !planPathWithinRoot(sourceRoot, source) {
		return errors.New("源路径越过下载根目录")
	}
	return validateExistingDirectoryAncestors(ctx, filepath.Dir(source), "源")
}

func validateExistingDirectoryAncestors(ctx context.Context, parent, kind string) error {
	ancestors := make([]string, 0, 16)
	for current := filepath.Clean(parent); ; current = filepath.Dir(current) {
		ancestors = append(ancestors, current)
		next := filepath.Dir(current)
		if samePlanPath(next, current) {
			break
		}
	}
	for left, right := 0, len(ancestors)-1; left < right; left, right = left+1, right-1 {
		ancestors[left], ancestors[right] = ancestors[right], ancestors[left]
	}
	for _, ancestor := range ancestors {
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := os.Lstat(ancestor)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("检查%s祖先 %s: %w", kind, ancestor, err)
		}
		if isUnsafePathLink(info) {
			return fmt.Errorf("%s祖先是链接或重解析点，拒绝访问: %s", kind, ancestor)
		}
		if !info.IsDir() {
			return fmt.Errorf("%s祖先不是目录: %s", kind, ancestor)
		}
	}
	return nil
}

func temporaryPath(target, planID string, index int) string {
	digest := sha256.Sum256([]byte(planID))
	return fmt.Sprintf("%s.moviepilot-%s-%d.tmp", target, hex.EncodeToString(digest[:8]), index)
}

func removeStaleTemporary(ctx context.Context, path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("检查临时目标: %w", err)
	}
	if info.IsDir() || (!info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0) {
		return errors.New("临时目标类型无效")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("清理临时目标: %w", err)
	}
	return nil
}

func verifyPlannedTarget(ctx context.Context, operation Operation) error {
	return verifyPlannedPath(ctx, operation.Target, operation)
}

func verifyPlannedPath(ctx context.Context, path string, operation Operation) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if operation.Mode == Symlink {
		if info.Mode()&os.ModeSymlink == 0 {
			return errors.New("目标不是软链接")
		}
		return verifyPlannedSymlink(ctx, path, operation, info)
	}
	if isUnsafePathLink(info) || !info.Mode().IsRegular() {
		return errors.New("目标不是普通文件")
	}
	return verifyFileFingerprintFromInfo(ctx, path, operation.ExpectedSize, operation.ExpectedSHA256, info)
}

func verifyPlannedSymlink(ctx context.Context, path string, operation Operation, linkInfo os.FileInfo) error {
	resolvedTarget, err := resolvedPlanPath(path)
	if err != nil {
		return errors.New("解析目标软链接失败")
	}
	resolvedSource, err := resolvedPlanPath(operation.Source)
	if err != nil {
		return errors.New("解析计划源文件失败")
	}
	if !samePlanPath(resolvedTarget, resolvedSource) {
		return errors.New("目标软链接未解析到计划源文件")
	}

	sourceInfo, err := os.Stat(operation.Source)
	if err != nil {
		return err
	}
	referentInfo, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !sourceInfo.Mode().IsRegular() || !referentInfo.Mode().IsRegular() || !os.SameFile(sourceInfo, referentInfo) {
		return errors.New("目标软链接未引用计划源文件")
	}
	if err := verifyFileFingerprintFromInfo(
		ctx, path, operation.ExpectedSize, operation.ExpectedSHA256, referentInfo,
	); err != nil {
		return err
	}

	finalLinkInfo, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if finalLinkInfo.Mode()&os.ModeSymlink == 0 || !os.SameFile(linkInfo, finalLinkInfo) {
		return errors.New("目标软链接在校验期间已被替换")
	}
	finalResolvedTarget, err := resolvedPlanPath(path)
	if err != nil || !samePlanPath(finalResolvedTarget, resolvedSource) {
		return errors.New("目标软链接在校验期间已改变")
	}
	finalSourceInfo, err := os.Stat(operation.Source)
	if err != nil || !os.SameFile(sourceInfo, finalSourceInfo) {
		return errors.New("计划源文件在软链接校验期间已改变")
	}
	return nil
}

func resolvedPlanPath(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	return normalizePlanPath(resolved)
}

func removeVerifiedMoveSource(ctx context.Context, boundary executionBoundary, operation Operation) error {
	if err := boundary.validateSource(ctx, operation.Source); err != nil {
		return err
	}
	if err := boundary.validateTarget(ctx, operation.Target); err != nil {
		return err
	}
	info, err := os.Lstat(operation.Source)
	if errors.Is(err, os.ErrNotExist) {
		return verifyPlannedTarget(ctx, operation)
	}
	if err != nil {
		return fmt.Errorf("删除前检查源文件: %w", err)
	}
	if isUnsafePathLink(info) || !info.Mode().IsRegular() {
		return errors.New("删除前源文件类型已改变")
	}
	if err := verifyFileFingerprintFromInfo(
		ctx, operation.Source, operation.ExpectedSize, operation.ExpectedSHA256, info,
	); err != nil {
		return fmt.Errorf("删除前源文件指纹已改变: %w", err)
	}
	if err := verifyPlannedTarget(ctx, operation); err != nil {
		return fmt.Errorf("删除前目标文件指纹已改变: %w", err)
	}
	if err := boundary.validateSource(ctx, operation.Source); err != nil {
		return err
	}
	if err := boundary.validateTarget(ctx, operation.Target); err != nil {
		return err
	}
	finalInfo, err := os.Lstat(operation.Source)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return verifyPlannedTarget(ctx, operation)
		}
		return fmt.Errorf("删除前复核源文件: %w", err)
	}
	if isUnsafePathLink(finalInfo) || !finalInfo.Mode().IsRegular() || !os.SameFile(info, finalInfo) {
		return errors.New("删除前源文件已被替换")
	}
	if err := verifyFileFingerprintFromInfo(
		ctx, operation.Source, operation.ExpectedSize, operation.ExpectedSHA256, finalInfo,
	); err != nil {
		return fmt.Errorf("删除前复核源文件指纹已改变: %w", err)
	}
	if err := boundary.validateTarget(ctx, operation.Target); err != nil {
		return err
	}
	if err := verifyPlannedTarget(ctx, operation); err != nil {
		return fmt.Errorf("删除前复核目标文件指纹已改变: %w", err)
	}
	if err := boundary.validateSource(ctx, operation.Source); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Remove(operation.Source); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return verifyPlannedTarget(ctx, operation)
		}
		return fmt.Errorf("删除已移动源文件: %w", err)
	}
	return nil
}

func copyFile(ctx context.Context, source, target string, mode os.FileMode) error {
	if ctx == nil {
		return errors.New("整理上下文无效")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	success := false
	defer func() {
		_ = output.Close()
		if !success {
			_ = os.Remove(target)
		}
	}()
	if _, err := io.Copy(output, contextReader{ctx: ctx, reader: input}); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := output.Sync(); err != nil {
		return err
	}
	if err := output.Close(); err != nil {
		return err
	}
	success = true
	return nil
}

// FingerprintFile 流式读取普通文件并返回其大小和小写 SHA-256。
func FingerprintFile(ctx context.Context, path string) (int64, string, error) {
	if ctx == nil {
		return 0, "", errors.New("整理上下文无效")
	}
	if err := ctx.Err(); err != nil {
		return 0, "", err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return 0, "", err
	}
	if isUnsafePathLink(info) || !info.Mode().IsRegular() {
		return 0, "", errors.New("文件不是普通文件")
	}
	return readFileFingerprint(ctx, path, nil, info)
}

func verifyFileFingerprintFromInfo(
	ctx context.Context,
	path string,
	expectedSize int64,
	expectedSHA256 string,
	expectedInfo os.FileInfo,
) error {
	actualSize, actualSHA256, err := readFileFingerprint(ctx, path, &expectedSize, expectedInfo)
	if err != nil {
		return err
	}
	if actualSize != expectedSize {
		return fmt.Errorf("文件大小为 %d，期望 %d", actualSize, expectedSize)
	}
	if actualSHA256 != expectedSHA256 {
		return errors.New("文件 SHA-256 不匹配")
	}
	return nil
}

func readFileFingerprint(
	ctx context.Context,
	path string,
	expectedSize *int64,
	expectedInfo os.FileInfo,
) (int64, string, error) {
	if ctx == nil {
		return 0, "", errors.New("整理上下文无效")
	}
	if err := ctx.Err(); err != nil {
		return 0, "", err
	}
	file, err := os.Open(path)
	if err != nil {
		return 0, "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return 0, "", err
	}
	if !info.Mode().IsRegular() {
		return 0, "", errors.New("文件不是普通文件")
	}
	if expectedInfo != nil && !os.SameFile(expectedInfo, info) {
		return 0, "", errors.New("文件在打开前已被替换")
	}
	// Windows 上 os.FileInfo 的文件标识是惰性读取的（loadFileId）：如果
	// FileInfo 是在文件被替换之前拿到的、直到这里才第一次比较，读到的其实
	// 是新文件的标识，os.SameFile 会把两个不同文件判成同一个。调用方没声明
	// 期望大小时，用计划时看到的文件大小兜底；同大小的替换由后续 SHA-256 兜住。
	if expectedInfo != nil && expectedSize == nil && expectedInfo.Size() != info.Size() {
		return 0, "", errors.New("文件在打开前已被替换")
	}
	if expectedSize != nil && info.Size() != *expectedSize {
		return 0, "", fmt.Errorf("文件大小为 %d，期望 %d", info.Size(), *expectedSize)
	}
	hash := sha256.New()
	written, err := io.Copy(hash, contextReader{ctx: ctx, reader: file})
	if err != nil {
		return 0, "", err
	}
	if written != info.Size() {
		return 0, "", fmt.Errorf("读取文件大小为 %d，初始大小 %d", written, info.Size())
	}
	finalInfo, err := file.Stat()
	if err != nil {
		return 0, "", err
	}
	if finalInfo.Size() != info.Size() {
		return 0, "", errors.New("校验期间文件大小发生变化")
	}
	return written, hex.EncodeToString(hash.Sum(nil)), nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(buffer)
}
