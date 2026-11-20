package organizer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writePlanFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("创建目录: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("写入文件: %v", err)
	}
}

func fileOperation(t *testing.T, source, target string, mode Mode) Operation {
	t.Helper()
	size, digest, err := FingerprintFile(context.Background(), source)
	if err != nil {
		t.Fatalf("计算指纹: %v", err)
	}
	return Operation{Source: source, Target: target, Mode: mode, ExpectedSize: size, ExpectedSHA256: digest}
}

// TestBuildPlanGuards 覆盖构建计划阶段的全部拒绝理由。
func TestBuildPlanGuards(t *testing.T) {
	root := t.TempDir()
	writePlanFile(t, filepath.Join(root, "src", "a.mkv"), "aaa")
	source := filepath.Join(root, "src", "a.mkv")
	valid := fileOperation(t, source, filepath.Join(root, "lib", "a.mkv"), Copy)

	if _, err := BuildPlan("", []Operation{valid}); err == nil || err.Error() != "整理根目录无效" {
		t.Fatalf("空根目录 = %v", err)
	}
	if _, err := BuildPlan(root, nil); err == nil || !strings.Contains(err.Error(), "整理计划文件数必须在") {
		t.Fatalf("空计划 = %v", err)
	}
	tooMany := make([]Operation, maxPlanOperations+1)
	if _, err := BuildPlan(root, tooMany); err == nil || !strings.Contains(err.Error(), "整理计划文件数必须在") {
		t.Fatalf("超量计划 = %v", err)
	}

	badMode := valid
	badMode.Mode = Mode("link")
	if _, err := BuildPlan(root, []Operation{badMode}); err == nil ||
		!strings.Contains(err.Error(), "不支持的整理方式") {
		t.Fatalf("非法方式 = %v", err)
	}

	negativeSize := valid
	negativeSize.ExpectedSize = -1
	if _, err := BuildPlan(root, []Operation{negativeSize}); err == nil ||
		!strings.Contains(err.Error(), "文件指纹无效") {
		t.Fatalf("负大小 = %v", err)
	}

	badDigest := valid
	badDigest.ExpectedSHA256 = strings.Repeat("g", 64)
	if _, err := BuildPlan(root, []Operation{badDigest}); err == nil ||
		!strings.Contains(err.Error(), "文件指纹无效") {
		t.Fatalf("非法摘要 = %v", err)
	}

	emptySource := valid
	emptySource.Source = ""
	if _, err := BuildPlan(root, []Operation{emptySource}); err == nil ||
		!strings.Contains(err.Error(), "源路径无效") {
		t.Fatalf("空源路径 = %v", err)
	}

	emptyTarget := valid
	emptyTarget.Target = ""
	if _, err := BuildPlan(root, []Operation{emptyTarget}); err == nil ||
		!strings.Contains(err.Error(), "目标路径无效") {
		t.Fatalf("空目标路径 = %v", err)
	}

	samePath := valid
	samePath.Target = source
	if _, err := BuildPlan(root, []Operation{samePath}); err == nil ||
		!strings.Contains(err.Error(), "不能相同") {
		t.Fatalf("同一路径 = %v", err)
	}

	outside := valid
	outside.Target = filepath.Join(filepath.Dir(root), "outside.mkv")
	if _, err := BuildPlan(root, []Operation{outside}); err == nil ||
		!strings.Contains(err.Error(), "越过整理根目录") {
		t.Fatalf("越界目标 = %v", err)
	}

	duplicate := []Operation{valid, valid}
	if _, err := BuildPlan(root, duplicate); err == nil || !strings.Contains(err.Error(), "目标路径重复") {
		t.Fatalf("重复目标 = %v", err)
	}

	plan, err := BuildPlan(root, []Operation{valid})
	if err != nil {
		t.Fatalf("合法计划: %v", err)
	}
	if len(plan.ID) != 32 || !filepath.IsAbs(plan.TargetRoot) || len(plan.Operations) != 1 {
		t.Fatalf("计划字段 = %+v", plan)
	}
	// 计划 ID 只由操作列表决定，重复构建结果一致。
	again, err := BuildPlan(root, []Operation{valid})
	if err != nil || again.ID != plan.ID {
		t.Fatalf("计划 ID 不确定: %v（%q vs %q）", err, plan.ID, again.ID)
	}
}

// TestExecutePlanGuards 覆盖执行阶段的上下文、计划与边界校验。
func TestExecutePlanGuards(t *testing.T) {
	root := t.TempDir()
	writePlanFile(t, filepath.Join(root, "src", "a.mkv"), "aaa")
	source := filepath.Join(root, "src", "a.mkv")
	plan, err := BuildPlan(root, []Operation{
		fileOperation(t, source, filepath.Join(root, "lib", "a.mkv"), Copy),
	})
	if err != nil {
		t.Fatalf("构建计划: %v", err)
	}

	if err := Execute(nil, plan); err == nil || err.Error() != "整理上下文无效" {
		t.Fatalf("空上下文 = %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Execute(cancelled, plan); !errors.Is(err, context.Canceled) {
		t.Fatalf("已取消上下文 = %v", err)
	}

	broken := plan
	broken.TargetRoot = ""
	if err := Execute(context.Background(), broken); err == nil || err.Error() != "整理计划无效" {
		t.Fatalf("空目标根 = %v", err)
	}
	empty := plan
	empty.Operations = nil
	if err := Execute(context.Background(), empty); err == nil || err.Error() != "整理计划无效" {
		t.Fatalf("空操作 = %v", err)
	}

	duplicate := plan
	duplicate.Operations = append(append([]Operation{}, plan.Operations...), plan.Operations[0])
	if err := Execute(context.Background(), duplicate); err == nil ||
		!strings.Contains(err.Error(), "目标路径重复") {
		t.Fatalf("重复目标 = %v", err)
	}

	if err := ExecuteWithinSourceRoot(context.Background(), "", plan); err == nil ||
		err.Error() != "整理源根目录无效" {
		t.Fatalf("空源根 = %v", err)
	}
	if err := ExecuteWithinSourceRoot(nil, root, plan); err == nil ||
		err.Error() != "整理上下文无效" {
		t.Fatalf("源根空上下文 = %v", err)
	}

	crossing := plan
	crossing.Operations = []Operation{fileOperation(t, source, filepath.Join(root, "lib", "a.mkv"), Copy)}
	if err := ExecuteWithinSourceRoot(context.Background(), filepath.Join(root, "lib"), crossing); err == nil ||
		!strings.Contains(err.Error(), "源路径越过下载根目录") {
		t.Fatalf("源路径越界 = %v", err)
	}

	if err := Execute(context.Background(), plan); err != nil {
		t.Fatalf("正常执行: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "lib", "a.mkv")); err != nil {
		t.Fatalf("目标未生成: %v", err)
	}
}

// TestExecuteMoveWithoutSourceIsIdempotent 覆盖「源已不存在但目标指纹匹配」的幂等重试。
func TestExecuteMoveWithoutSourceIsIdempotent(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "src", "a.mkv")
	target := filepath.Join(root, "lib", "a.mkv")
	writePlanFile(t, source, "aaa")
	operation := fileOperation(t, source, target, Move)
	plan, err := BuildPlan(root, []Operation{operation})
	if err != nil {
		t.Fatalf("构建计划: %v", err)
	}
	// 第一次执行：移动成功。
	if err := Execute(context.Background(), plan); err != nil {
		t.Fatalf("首次移动: %v", err)
	}
	if _, err := os.Stat(source); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("源文件应已删除: %v", err)
	}
	// 第二次执行：源不存在、目标指纹一致，应视为成功。
	if err := Execute(context.Background(), plan); err != nil {
		t.Fatalf("幂等重试: %v", err)
	}
}

// TestVerifyPlannedPathBranches 覆盖目标校验的类型与软链接分支。
func TestVerifyPlannedPathBranches(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "src.mkv")
	writePlanFile(t, source, "aaa")
	operation := fileOperation(t, source, filepath.Join(root, "lib.mkv"), Symlink)

	// 目标不是软链接。
	regular := filepath.Join(root, "regular.mkv")
	writePlanFile(t, regular, "aaa")
	if err := verifyPlannedPath(context.Background(), regular, operation); err == nil ||
		!strings.Contains(err.Error(), "目标不是软链接") {
		t.Fatalf("普通文件当软链接 = %v", err)
	}

	// 目标不是普通文件（复制模式但目标是目录）。
	directory := filepath.Join(root, "dir")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatalf("创建目录: %v", err)
	}
	copyOperation := operation
	copyOperation.Mode = Copy
	if err := verifyPlannedPath(context.Background(), directory, copyOperation); err == nil ||
		!strings.Contains(err.Error(), "目标不是普通文件") {
		t.Fatalf("目录当普通文件 = %v", err)
	}

	// 指纹不一致。
	mismatch := operation
	mismatch.ExpectedSHA256 = strings.Repeat("0", 64)
	if err := verifyPlannedPath(context.Background(), regular, copyOperationWithDigest(mismatch)); err == nil ||
		!strings.Contains(err.Error(), "SHA-256 不匹配") {
		t.Fatalf("指纹不一致 = %v", err)
	}

	// 指向其它文件的软链接。
	other := filepath.Join(root, "other.mkv")
	writePlanFile(t, other, "aaa")
	wrongLink := filepath.Join(root, "wrong-link.mkv")
	createFileSymlinkOrSkip(t, other, wrongLink)
	if err := verifyPlannedPath(context.Background(), wrongLink, operation); err == nil ||
		!strings.Contains(err.Error(), "未解析到计划源文件") {
		t.Fatalf("错误软链接 = %v", err)
	}

	// 目标软链接本身不存在。
	dangling := filepath.Join(root, "dangling.mkv")
	if err := verifyPlannedPath(context.Background(), dangling, operation); err == nil {
		t.Fatal("不存在的目标应报错")
	}

	// 正确软链接。
	goodLink := filepath.Join(root, "good-link.mkv")
	createFileSymlinkOrSkip(t, source, goodLink)
	if err := verifyPlannedPath(context.Background(), goodLink, operation); err != nil {
		t.Fatalf("正确软链接: %v", err)
	}

	// 源文件缺失时解析失败。
	missingSource := operation
	missingSource.Source = filepath.Join(root, "nope.mkv")
	if err := verifyPlannedPath(context.Background(), goodLink, missingSource); err == nil {
		t.Fatal("源缺失应报错")
	}

	// 上下文已取消。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := verifyPlannedPath(ctx, goodLink, operation); err == nil {
		t.Fatal("取消上下文应报错")
	}
}

func copyOperationWithDigest(operation Operation) Operation {
	operation.Mode = Copy
	return operation
}

// TestRemoveVerifiedMoveSourceGuards 覆盖移动后删源的校验与取消。
func TestRemoveVerifiedMoveSourceGuards(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "src.mkv")
	target := filepath.Join(root, "lib.mkv")
	writePlanFile(t, source, "aaa")
	writePlanFile(t, target, "aaa")
	operation := fileOperation(t, source, target, Move)
	boundary := executionBoundary{targetRoot: root}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := removeVerifiedMoveSource(ctx, boundary, operation); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消上下文 = %v", err)
	}

	// 源已不存在但目标匹配：视为成功。
	if err := os.Remove(source); err != nil {
		t.Fatalf("删除源: %v", err)
	}
	if err := removeVerifiedMoveSource(context.Background(), boundary, operation); err != nil {
		t.Fatalf("源缺失幂等 = %v", err)
	}

	// 源指纹不匹配时报错。
	writePlanFile(t, source, "different")
	if err := removeVerifiedMoveSource(context.Background(), boundary, operation); err == nil ||
		!strings.Contains(err.Error(), "指纹已改变") {
		t.Fatalf("源指纹不匹配 = %v", err)
	}
}

// TestCopyFileGuards 覆盖复制函数的上下文与路径错误。
func TestCopyFileGuards(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "a.mkv")
	writePlanFile(t, source, "aaa")

	if err := copyFile(nil, source, filepath.Join(root, "b.mkv"), 0o600); err == nil ||
		err.Error() != "整理上下文无效" {
		t.Fatalf("空上下文 = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := copyFile(ctx, source, filepath.Join(root, "c.mkv"), 0o600); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消上下文 = %v", err)
	}
	if err := copyFile(context.Background(), filepath.Join(root, "missing"), filepath.Join(root, "d.mkv"), 0o600); err == nil {
		t.Fatal("源缺失应报错")
	}
	existing := filepath.Join(root, "e.mkv")
	writePlanFile(t, existing, "old")
	if err := copyFile(context.Background(), source, existing, 0o600); err == nil {
		t.Fatal("目标已存在应报错")
	}
	if err := copyFile(context.Background(), source, filepath.Join(root, "f.mkv"), 0o600); err != nil {
		t.Fatalf("正常复制: %v", err)
	}
}
