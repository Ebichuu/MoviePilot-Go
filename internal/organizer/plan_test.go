package organizer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestBuildPlanValidatesFingerprintsAndPaths(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	targetRoot := filepath.Join(root, "library")
	source := filepath.Join(root, "source.mkv")
	target := filepath.Join(targetRoot, "source.mkv")
	size, digest := testFingerprint([]byte("video"))

	tests := []struct {
		name      string
		operation Operation
	}{
		{
			name: "negative size",
			operation: Operation{
				Source: source, Target: target, Mode: Copy,
				ExpectedSize: -1, ExpectedSHA256: digest,
			},
		},
		{
			name: "short digest",
			operation: Operation{
				Source: source, Target: target, Mode: Copy,
				ExpectedSize: size, ExpectedSHA256: digest[:63],
			},
		},
		{
			name: "uppercase digest",
			operation: Operation{
				Source: source, Target: target, Mode: Copy,
				ExpectedSize: size, ExpectedSHA256: strings.ToUpper(digest),
			},
		},
		{
			name: "non hexadecimal digest",
			operation: Operation{
				Source: source, Target: target, Mode: Copy,
				ExpectedSize: size, ExpectedSHA256: strings.Repeat("g", sha256.Size*2),
			},
		},
		{
			name: "same source and target",
			operation: Operation{
				Source: target, Target: target, Mode: Move,
				ExpectedSize: size, ExpectedSHA256: digest,
			},
		},
		{
			name: "path exceeds bound",
			operation: Operation{
				Source: filepath.Join(root, strings.Repeat("a", maxPlanPathLength)), Target: target, Mode: Copy,
				ExpectedSize: size, ExpectedSHA256: digest,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := BuildPlan(targetRoot, []Operation{tt.operation}); err == nil {
				t.Fatal("BuildPlan() error = nil")
			}
		})
	}
}

func TestBuildPlanRejectsWindowsCaseInsensitiveDuplicateTargets(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows 路径比较语义测试")
	}
	t.Parallel()
	root := t.TempDir()
	targetRoot := filepath.Join(root, "library")
	size, digest := testFingerprint([]byte("video"))
	_, err := BuildPlan(targetRoot, []Operation{
		{
			Source: filepath.Join(root, "source-a.mkv"), Target: filepath.Join(targetRoot, "Movie.mkv"), Mode: Copy,
			ExpectedSize: size, ExpectedSHA256: digest,
		},
		{
			Source: filepath.Join(root, "source-b.mkv"), Target: filepath.Join(targetRoot, "movie.mkv"), Mode: Copy,
			ExpectedSize: size, ExpectedSHA256: digest,
		},
	})
	if err == nil {
		t.Fatal("Windows 大小写不同但同一目标的计划应被拒绝")
	}
	forged := Plan{
		ID:         "forged",
		TargetRoot: targetRoot,
		Operations: []Operation{
			{
				Source: filepath.Join(root, "source-a.mkv"), Target: filepath.Join(targetRoot, "Movie.mkv"), Mode: Copy,
				ExpectedSize: size, ExpectedSHA256: digest,
			},
			{
				Source: filepath.Join(root, "source-b.mkv"), Target: filepath.Join(targetRoot, "movie.mkv"), Mode: Copy,
				ExpectedSize: size, ExpectedSHA256: digest,
			},
		},
	}
	if err := Execute(t.Context(), forged); err == nil {
		t.Fatal("Execute() 应拒绝 Windows 大小写重复目标的伪造计划")
	}
}

func TestExecuteCopyIsIdempotentAndRejectsConflict(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	source := filepath.Join(root, "source.mkv")
	targetRoot := filepath.Join(root, "library")
	target := filepath.Join(targetRoot, "Movie", "movie.mkv")
	content := []byte("video")
	if err := os.WriteFile(source, content, 0o644); err != nil {
		t.Fatal(err)
	}
	plan := mustTestPlan(t, targetRoot, source, target, Copy, content)
	if err := Execute(t.Context(), plan); err != nil {
		t.Fatal(err)
	}
	if err := Execute(t.Context(), plan); err != nil {
		t.Fatalf("幂等重试失败: %v", err)
	}
	if err := os.WriteFile(target, []byte("other"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Execute(t.Context(), plan); err == nil {
		t.Fatal("不同内容的目标不得覆盖")
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "other" {
		t.Fatalf("冲突目标被改写: %q, %v", got, err)
	}
}

func TestExecuteHardlinkIsIdempotent(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	source := filepath.Join(root, "source.mkv")
	targetRoot := filepath.Join(root, "library")
	target := filepath.Join(targetRoot, "movie.mkv")
	content := []byte("video")
	if err := os.WriteFile(source, content, 0o644); err != nil {
		t.Fatal(err)
	}
	plan := mustTestPlan(t, targetRoot, source, target, Hardlink, content)
	if err := Execute(t.Context(), plan); err != nil {
		t.Fatal(err)
	}
	if err := Execute(t.Context(), plan); err != nil {
		t.Fatalf("硬链接幂等重试失败: %v", err)
	}
	sourceInfo, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	targetInfo, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(sourceInfo, targetInfo) {
		t.Fatal("Hardlink 目标未引用计划源文件")
	}
}

func TestExecuteSymlinkIsIdempotent(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	source := filepath.Join(root, "source.mkv")
	targetRoot := filepath.Join(root, "library")
	target := filepath.Join(targetRoot, "movie.mkv")
	content := []byte("video")
	if err := os.WriteFile(source, content, 0o644); err != nil {
		t.Fatal(err)
	}
	requireFileSymlinkSupport(t, source, filepath.Join(root, "probe-link"))
	plan := mustTestPlan(t, targetRoot, source, target, Symlink, content)
	if err := Execute(t.Context(), plan); err != nil {
		t.Fatal(err)
	}
	if err := Execute(t.Context(), plan); err != nil {
		t.Fatalf("软链接幂等重试失败: %v", err)
	}
	resolved, err := resolvedPlanPath(target)
	if err != nil {
		t.Fatal(err)
	}
	wantResolved, err := resolvedPlanPath(source)
	if err != nil {
		t.Fatal(err)
	}
	if !samePlanPath(resolved, wantResolved) {
		t.Fatalf("软链接解析为 %q，期望 %q", resolved, wantResolved)
	}
}

func TestExecuteSymlinkRejectsTamperedReferent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name             string
		alternateContent []byte
	}{
		{name: "相同指纹但路径不同", alternateContent: []byte("video")},
		{name: "指纹不同", alternateContent: []byte("other")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			source := filepath.Join(root, "source.mkv")
			alternate := filepath.Join(root, "alternate.mkv")
			targetRoot := filepath.Join(root, "library")
			target := filepath.Join(targetRoot, "movie.mkv")
			content := []byte("video")
			if err := os.WriteFile(source, content, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(alternate, tt.alternateContent, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(targetRoot, 0o755); err != nil {
				t.Fatal(err)
			}
			createFileSymlinkOrSkip(t, alternate, target)
			plan := mustTestPlan(t, targetRoot, source, target, Symlink, content)
			if err := Execute(t.Context(), plan); err == nil {
				t.Fatal("被篡改的软链接目标不得作为幂等结果接受")
			}
			resolved, err := resolvedPlanPath(target)
			if err != nil {
				t.Fatal(err)
			}
			wantResolved, err := resolvedPlanPath(alternate)
			if err != nil {
				t.Fatal(err)
			}
			if !samePlanPath(resolved, wantResolved) {
				t.Fatalf("拒绝篡改后软链接被覆盖为 %q", resolved)
			}
		})
	}
}

func TestExecuteConcurrentTargetClaimNeverOverwritesWinner(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	targetRoot := filepath.Join(root, "library")
	target := filepath.Join(targetRoot, "movie.mkv")
	type contender struct {
		content []byte
		plan    Plan
	}
	contenders := []contender{
		{content: []byte("first")},
		{content: []byte("other")},
	}
	for index := range contenders {
		source := filepath.Join(root, "source-"+string(rune('a'+index))+".mkv")
		if err := os.WriteFile(source, contenders[index].content, 0o644); err != nil {
			t.Fatal(err)
		}
		contenders[index].plan = mustTestPlan(
			t, targetRoot, source, target, Copy, contenders[index].content,
		)
	}

	type result struct {
		content []byte
		err     error
	}
	start := make(chan struct{})
	results := make(chan result, len(contenders))
	for _, contender := range contenders {
		contender := contender
		go func() {
			<-start
			results <- result{content: contender.content, err: Execute(t.Context(), contender.plan)}
		}()
	}
	close(start)

	gotResults := make([]result, 0, len(contenders))
	for range contenders {
		gotResults = append(gotResults, <-results)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	successes := 0
	for _, result := range gotResults {
		if result.err == nil {
			successes++
			if string(got) != string(result.content) {
				t.Fatalf("成功执行者内容 %q 被另一执行者覆盖为 %q", result.content, got)
			}
		}
	}
	if successes != 1 {
		t.Fatalf("并发竞争成功数 = %d，期望 1；结果 = %#v", successes, gotResults)
	}
}

func TestPublishNoReplacePreservesExistingTarget(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		existing []byte
	}{
		{name: "相同内容", existing: []byte("video")},
		{name: "不同内容", existing: []byte("other")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			temporary := filepath.Join(root, "temporary")
			target := filepath.Join(root, "target")
			if err := os.WriteFile(temporary, []byte("video"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(target, tt.existing, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := publishNoReplace(temporary, target); err == nil {
				t.Fatal("no-replace 发布不得替换现有目标")
			}
			got, err := os.ReadFile(target)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(tt.existing) {
				t.Fatalf("现有目标被改写为 %q", got)
			}
			if _, err := os.Stat(temporary); err != nil {
				t.Fatalf("发布失败时临时文件不应消失: %v", err)
			}
		})
	}
}

func TestExecuteMoveCrashReplay(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	source := filepath.Join(root, "source.mkv")
	targetRoot := filepath.Join(root, "library")
	target := filepath.Join(targetRoot, "source.mkv")
	content := []byte("video")
	if err := os.WriteFile(source, content, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, content, 0o644); err != nil {
		t.Fatal(err)
	}
	plan := mustTestPlan(t, targetRoot, source, target, Move, content)

	// 模拟临时文件已 rename 为目标、但进程尚未 remove 源文件即崩溃。
	if err := Execute(t.Context(), plan); err != nil {
		t.Fatalf("rename 后重放失败: %v", err)
	}
	if _, err := os.Stat(source); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("匹配目标存在时源文件未删除: %v", err)
	}

	// 模拟源文件已删除、但数据库尚未提交完成状态即崩溃。
	if err := Execute(t.Context(), plan); err != nil {
		t.Fatalf("remove 后重放失败: %v", err)
	}
}

func TestExecuteCleansInterruptedTemporaryFileBeforeReplay(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	source := filepath.Join(root, "source.mkv")
	targetRoot := filepath.Join(root, "library")
	target := filepath.Join(targetRoot, "source.mkv")
	content := []byte("video")
	if err := os.WriteFile(source, content, 0o644); err != nil {
		t.Fatal(err)
	}
	plan := mustTestPlan(t, targetRoot, source, target, Copy, content)
	temporary := temporaryPath(target, plan.ID, 0)
	if err := os.MkdirAll(filepath.Dir(temporary), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(temporary, []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Execute(t.Context(), plan); err != nil {
		t.Fatalf("残留临时文件后的整理重放失败: %v", err)
	}
	if _, err := os.Stat(temporary); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("残留临时文件未清理: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(content) {
		t.Fatalf("重放目标内容 = %q, want %q", got, content)
	}
}

func TestExecuteMoveReplayRejectsTamperedTarget(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	source := filepath.Join(root, "source.mkv")
	targetRoot := filepath.Join(root, "library")
	target := filepath.Join(targetRoot, "source.mkv")
	content := []byte("video")
	if err := os.WriteFile(source, content, 0o644); err != nil {
		t.Fatal(err)
	}
	plan := mustTestPlan(t, targetRoot, source, target, Move, content)
	if err := Execute(t.Context(), plan); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("other"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Execute(t.Context(), plan); err == nil {
		t.Fatal("源文件缺失时不得接受被篡改的目标")
	}
}

func TestExecuteMoveDoesNotDeleteChangedSourceWhenTargetMatches(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	source := filepath.Join(root, "source.mkv")
	targetRoot := filepath.Join(root, "library")
	target := filepath.Join(targetRoot, "source.mkv")
	expected := []byte("video")
	changed := []byte("other")
	if err := os.WriteFile(source, changed, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(targetRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, expected, 0o644); err != nil {
		t.Fatal(err)
	}
	plan := mustTestPlan(t, targetRoot, source, target, Move, expected)
	if err := Execute(t.Context(), plan); err == nil {
		t.Fatal("目标匹配时也必须先拒绝已变化的源文件")
	}
	if got, err := os.ReadFile(source); err != nil || string(got) != string(changed) {
		t.Fatalf("已变化的源文件被删除或改写: %q, %v", got, err)
	}
}

func TestExecuteMoveMissingSourceRequiresExactTargetFingerprint(t *testing.T) {
	t.Parallel()
	content := []byte("video")
	tests := []struct {
		name          string
		targetContent []byte
		createTarget  bool
	}{
		{name: "target missing"},
		{name: "same size different hash", targetContent: []byte("other"), createTarget: true},
		{name: "different size", targetContent: []byte("arbitrary"), createTarget: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			source := filepath.Join(root, "missing.mkv")
			targetRoot := filepath.Join(root, "library")
			target := filepath.Join(targetRoot, "missing.mkv")
			if tt.createTarget {
				if err := os.MkdirAll(targetRoot, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(target, tt.targetContent, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			plan := mustTestPlan(t, targetRoot, source, target, Move, content)
			if err := Execute(t.Context(), plan); err == nil {
				t.Fatal("缺失源文件不应接受任意目标")
			}
		})
	}
}

func TestExecuteVerifiesSourceBeforeSideEffects(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	source := filepath.Join(root, "source.mkv")
	targetRoot := filepath.Join(root, "library")
	target := filepath.Join(targetRoot, "source.mkv")
	if err := os.WriteFile(source, []byte("other"), 0o644); err != nil {
		t.Fatal(err)
	}
	plan := mustTestPlan(t, targetRoot, source, target, Copy, []byte("video"))
	if err := Execute(t.Context(), plan); err == nil {
		t.Fatal("源指纹不匹配应失败")
	}
	if _, err := os.Stat(targetRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("源指纹校验前产生了目标副作用: %v", err)
	}
}

func TestExecuteWithinSourceRootAllowsDirectoryAndSingleFileRoots(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		singleFile bool
	}{
		{name: "目录根目录"},
		{name: "单文件内容根", singleFile: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			downloadRoot := filepath.Join(root, "downloads")
			if err := os.MkdirAll(downloadRoot, 0o755); err != nil {
				t.Fatal(err)
			}
			source := filepath.Join(downloadRoot, "source.mkv")
			content := []byte("video")
			if err := os.WriteFile(source, content, 0o644); err != nil {
				t.Fatal(err)
			}
			sourceRoot := downloadRoot
			if tt.singleFile {
				sourceRoot = source
			}
			targetRoot := filepath.Join(root, "library")
			plan := mustTestPlan(t, targetRoot, source, filepath.Join(targetRoot, "movie.mkv"), Copy, content)
			if err := ExecuteWithinSourceRoot(t.Context(), sourceRoot, plan); err != nil {
				t.Fatalf("ExecuteWithinSourceRoot() error = %v", err)
			}
		})
	}
}

func TestExecuteWithinSourceRootRejectsBoundaryViolations(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		setup func(t *testing.T, root string) (sourceRoot string, source string)
	}{
		{
			name: "源文件在根目录外",
			setup: func(t *testing.T, root string) (string, string) {
				t.Helper()
				sourceRoot := filepath.Join(root, "downloads")
				if err := os.MkdirAll(sourceRoot, 0o755); err != nil {
					t.Fatal(err)
				}
				return sourceRoot, filepath.Join(root, "outside.mkv")
			},
		},
		{
			name: "源文件祖先是链接",
			setup: func(t *testing.T, root string) (string, string) {
				t.Helper()
				sourceRoot := filepath.Join(root, "downloads")
				external := filepath.Join(root, "external")
				if err := os.MkdirAll(sourceRoot, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(external, 0o755); err != nil {
					t.Fatal(err)
				}
				linked := filepath.Join(sourceRoot, "linked")
				createDirectorySymlinkOrSkip(t, external, linked)
				return sourceRoot, filepath.Join(linked, "source.mkv")
			},
		},
		{
			name: "源根目录本身是链接",
			setup: func(t *testing.T, root string) (string, string) {
				t.Helper()
				external := filepath.Join(root, "external")
				if err := os.MkdirAll(external, 0o755); err != nil {
					t.Fatal(err)
				}
				sourceRoot := filepath.Join(root, "downloads")
				createDirectorySymlinkOrSkip(t, external, sourceRoot)
				return sourceRoot, filepath.Join(sourceRoot, "source.mkv")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			sourceRoot, source := tt.setup(t, root)
			content := []byte("video")
			if err := os.WriteFile(source, content, 0o644); err != nil {
				t.Fatal(err)
			}
			targetRoot := filepath.Join(root, "library")
			target := filepath.Join(targetRoot, "movie.mkv")
			plan := mustTestPlan(t, targetRoot, source, target, Copy, content)
			if err := ExecuteWithinSourceRoot(t.Context(), sourceRoot, plan); err == nil {
				t.Fatal("不安全的源边界应被拒绝")
			}
			if _, err := os.Stat(targetRoot); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("拒绝源路径后产生了目标副作用: %v", err)
			}
		})
	}
}

func TestExecuteRejectsForgedSameSourceAndTarget(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := filepath.Join(root, "movie.mkv")
	content := []byte("video")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	size, digest := testFingerprint(content)
	plan := Plan{
		ID:         "forged",
		TargetRoot: root,
		Operations: []Operation{{
			Source: path, Target: path, Mode: Move,
			ExpectedSize: size, ExpectedSHA256: digest,
		}},
	}
	if err := Execute(t.Context(), plan); err == nil {
		t.Fatal("Execute() 应拒绝源目标相同的伪造计划")
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != string(content) {
		t.Fatalf("拒绝伪造计划后文件被修改: %q, %v", got, err)
	}
}

func TestExecuteRejectsLinkedTargetAncestors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		paths func(root string) (targetRoot string, linkedDirectory string, target string)
	}{
		{
			name: "TargetRoot 下方",
			paths: func(root string) (string, string, string) {
				targetRoot := filepath.Join(root, "library")
				linked := filepath.Join(targetRoot, "Movie")
				return targetRoot, linked, filepath.Join(linked, "movie.mkv")
			},
		},
		{
			name: "TargetRoot 本身",
			paths: func(root string) (string, string, string) {
				targetRoot := filepath.Join(root, "library")
				return targetRoot, targetRoot, filepath.Join(targetRoot, "movie.mkv")
			},
		},
		{
			name: "TargetRoot 上方",
			paths: func(root string) (string, string, string) {
				linked := filepath.Join(root, "linked-parent")
				targetRoot := filepath.Join(linked, "library")
				return targetRoot, linked, filepath.Join(targetRoot, "movie.mkv")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			source := filepath.Join(root, "source.mkv")
			externalRoot := filepath.Join(root, "external")
			content := []byte("video")
			if err := os.WriteFile(source, content, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(externalRoot, 0o755); err != nil {
				t.Fatal(err)
			}
			targetRoot, linkedDirectory, target := tt.paths(root)
			if parent := filepath.Dir(linkedDirectory); !samePlanPath(parent, root) {
				if err := os.MkdirAll(parent, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			createDirectorySymlinkOrSkip(t, externalRoot, linkedDirectory)
			plan := mustTestPlan(t, targetRoot, source, target, Copy, content)
			if err := Execute(t.Context(), plan); err == nil {
				t.Fatal("链接或重解析点目标祖先应被拒绝")
			}
			entries, err := os.ReadDir(externalRoot)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("目标写入越过整理根目录: %#v", entries)
			}
			if _, err := os.Stat(source); err != nil {
				t.Fatalf("拒绝链接祖先时源文件被修改: %v", err)
			}
		})
	}
}

func TestBuildPlanRejectsEscapingTarget(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	size, digest := testFingerprint([]byte("video"))
	_, err := BuildPlan(filepath.Join(root, "library"), []Operation{{
		Source: filepath.Join(root, "source"),
		Target: filepath.Join(root, "outside"),
		Mode:   Copy, ExpectedSize: size, ExpectedSHA256: digest,
	}})
	if err == nil {
		t.Fatal("越过目标根目录的计划应被拒绝")
	}
}

func TestExecuteRejectsNilAndCanceledContext(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	source := filepath.Join(root, "source.mkv")
	targetRoot := filepath.Join(root, "library")
	content := []byte("video")
	if err := os.WriteFile(source, content, 0o644); err != nil {
		t.Fatal(err)
	}
	plan := mustTestPlan(t, targetRoot, source, filepath.Join(targetRoot, "source.mkv"), Copy, content)
	if err := Execute(nil, plan); err == nil {
		t.Fatal("Execute(nil) error = nil")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Execute(ctx, plan); !errors.Is(err, context.Canceled) {
		t.Fatalf("Execute(canceled) error = %v, want context.Canceled", err)
	}
	if err := ExecuteWithinSourceRoot(nil, root, plan); err == nil {
		t.Fatal("ExecuteWithinSourceRoot(nil) error = nil")
	}
	if err := ExecuteWithinSourceRoot(ctx, "", plan); !errors.Is(err, context.Canceled) {
		t.Fatalf("ExecuteWithinSourceRoot(canceled) error = %v, want context.Canceled", err)
	}
}

func TestFingerprintFile(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := filepath.Join(root, "source.mkv")
	content := []byte("video")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	wantSize, wantSHA256 := testFingerprint(content)
	gotSize, gotSHA256, err := FingerprintFile(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	if gotSize != wantSize || gotSHA256 != wantSHA256 {
		t.Fatalf("FingerprintFile() = (%d, %q), want (%d, %q)", gotSize, gotSHA256, wantSize, wantSHA256)
	}
	if _, _, err := FingerprintFile(t.Context(), root); err == nil {
		t.Fatal("目录不得作为普通文件计算指纹")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := FingerprintFile(ctx, path); !errors.Is(err, context.Canceled) {
		t.Fatalf("FingerprintFile(canceled) error = %v, want context.Canceled", err)
	}
}

func TestCopyFileCancellationRemovesPartialTarget(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	source := filepath.Join(root, "source.mkv")
	target := filepath.Join(root, "target.tmp")
	if err := os.WriteFile(source, []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := copyFile(ctx, source, target, 0o644); !errors.Is(err, context.Canceled) {
		t.Fatalf("copyFile() error = %v, want context.Canceled", err)
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial target still exists: %v", err)
	}
}

func mustTestPlan(t *testing.T, targetRoot, source, target string, mode Mode, content []byte) Plan {
	t.Helper()
	size, digest := testFingerprint(content)
	plan, err := BuildPlan(targetRoot, []Operation{{
		Source: source, Target: target, Mode: mode,
		ExpectedSize: size, ExpectedSHA256: digest,
	}})
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func testFingerprint(content []byte) (int64, string) {
	digest := sha256.Sum256(content)
	return int64(len(content)), hex.EncodeToString(digest[:])
}

func requireFileSymlinkSupport(t *testing.T, target, link string) {
	t.Helper()
	createFileSymlinkOrSkip(t, target, link)
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
}

func createFileSymlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("当前 Windows 环境不能创建文件软链接: %v", err)
		}
		t.Fatal(err)
	}
}

func createDirectorySymlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("当前 Windows 环境不能创建目录软链接: %v", err)
		}
		t.Fatal(err)
	}
}
