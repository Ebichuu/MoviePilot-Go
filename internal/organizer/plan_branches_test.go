package organizer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCopyFileBranches(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source")
	if err := os.WriteFile(source, []byte("data"), 0o640); err != nil {
		t.Fatal(err)
	}

	if err := copyFile(nil, source, filepath.Join(dir, "nil-ctx"), 0o644); err == nil {
		t.Fatal("nil 上下文应报错")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := copyFile(canceled, source, filepath.Join(dir, "canceled"), 0o644); !errors.Is(err, context.Canceled) {
		t.Fatalf("已取消上下文 = %v", err)
	}
	if err := copyFile(context.Background(), filepath.Join(dir, "missing"), filepath.Join(dir, "missing-out"), 0o644); err == nil {
		t.Fatal("源文件不存在应报错")
	}

	existing := filepath.Join(dir, "existing")
	if err := os.WriteFile(existing, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(context.Background(), source, existing, 0o644); err == nil {
		t.Fatal("目标已存在应报错")
	}
	if content, err := os.ReadFile(existing); err != nil || string(content) != "keep" {
		t.Fatalf("既有目标被改写: %q %v", content, err)
	}

	target := filepath.Join(dir, "copied")
	if err := copyFile(context.Background(), source, target, 0o640); err != nil {
		t.Fatalf("复制失败: %v", err)
	}
	content, err := os.ReadFile(target)
	if err != nil || string(content) != "data" {
		t.Fatalf("复制内容 = %q %v", content, err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm&0o600 != 0o600 {
		t.Fatalf("权限 = %v", perm)
	}
}

func TestContextReaderCanceled(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (contextReader{ctx: canceled, reader: strings.NewReader("hello")}).Read(make([]byte, 8)); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消读取 = %v", err)
	}
	buffer := make([]byte, 8)
	count, err := (contextReader{ctx: context.Background(), reader: strings.NewReader("hello")}).Read(buffer)
	if err != nil || count != 5 || string(buffer[:count]) != "hello" {
		t.Fatalf("正常读取 = (%d, %v)", count, err)
	}
}

func TestRemoveStaleTemporaryBranches(t *testing.T) {
	dir := t.TempDir()
	if err := removeStaleTemporary(context.Background(), filepath.Join(dir, "absent")); err != nil {
		t.Fatalf("不存在应视为已清理: %v", err)
	}

	file := filepath.Join(dir, "stale.tmp")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeStaleTemporary(context.Background(), file); err != nil {
		t.Fatalf("清理失败: %v", err)
	}
	if _, err := os.Lstat(file); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("临时文件未删除")
	}

	directory := filepath.Join(dir, "temp-dir")
	if err := os.Mkdir(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := removeStaleTemporary(context.Background(), directory); err == nil ||
		!strings.Contains(err.Error(), "临时目标类型无效") {
		t.Fatalf("目录临时目标 = %v", err)
	}

	canceledFile := filepath.Join(dir, "canceled.tmp")
	if err := os.WriteFile(canceledFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := removeStaleTemporary(canceled, canceledFile); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消清理 = %v", err)
	}
	if _, err := os.Lstat(canceledFile); err != nil {
		t.Fatal("取消后不应删除临时文件")
	}

	// 父路径是文件时：Linux 上 Lstat 返回 ENOTDIR，被包成「检查临时目标」；
	// Windows 把 ERROR_PATH_NOT_FOUND 映射成 ErrNotExist，等于「这个临时文件不存在」，
	// 直接返回 nil 才是对的。
	blocking := filepath.Join(dir, "blocking")
	if err := os.WriteFile(blocking, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	blockedErr := removeStaleTemporary(context.Background(), filepath.Join(blocking, "child.tmp"))
	if runtime.GOOS == "windows" {
		if blockedErr != nil {
			t.Fatalf("Windows 上父路径是文件应视为不存在: %v", blockedErr)
		}
	} else if blockedErr == nil || !strings.Contains(blockedErr.Error(), "检查临时目标") {
		t.Fatalf("检查临时目标失败 = %v", blockedErr)
	}
}

func TestFingerprintFileBranches(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file")
	content := []byte("fingerprint")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, _, err := FingerprintFile(nil, path); err == nil {
		t.Fatal("nil 上下文应报错")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := FingerprintFile(canceled, path); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消指纹 = %v", err)
	}
	if _, _, err := FingerprintFile(context.Background(), filepath.Join(dir, "missing")); err == nil {
		t.Fatal("文件不存在应报错")
	}
	if _, _, err := FingerprintFile(context.Background(), dir); err == nil {
		t.Fatal("目录不是普通文件")
	}
	link := filepath.Join(dir, "link")
	createFileSymlinkOrSkip(t, path, link)
	if _, _, err := FingerprintFile(context.Background(), link); err == nil {
		t.Fatal("软链接不是普通文件")
	}

	size, digest, err := FingerprintFile(context.Background(), path)
	wantSize, wantDigest := testFingerprint(content)
	if err != nil || size != wantSize || digest != wantDigest {
		t.Fatalf("指纹 = (%d, %q, %v)", size, digest, err)
	}

	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyFileFingerprintFromInfo(context.Background(), path, size+1, digest, info); err == nil ||
		!strings.Contains(err.Error(), "文件大小为") {
		t.Fatalf("大小不匹配 = %v", err)
	}
	if err := verifyFileFingerprintFromInfo(context.Background(), path, size, strings.Repeat("0", 64), info); err == nil ||
		!strings.Contains(err.Error(), "SHA-256 不匹配") {
		t.Fatalf("摘要不匹配 = %v", err)
	}
	if err := verifyFileFingerprintFromInfo(canceled, path, size, digest, info); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消校验 = %v", err)
	}
}

func TestExecuteOperationRejections(t *testing.T) {
	targetRoot := t.TempDir()
	sourceRoot := t.TempDir()
	boundary := executionBoundary{targetRoot: targetRoot, sourceRoot: sourceRoot, enforceSourceRoot: true}

	// 源是目录。
	directory := filepath.Join(sourceRoot, "dir")
	if err := os.Mkdir(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	sourceOperation := Operation{Source: directory, Target: filepath.Join(targetRoot, "out"), Mode: Copy}
	if err := executeOperation(context.Background(), boundary, sourceOperation, "id", 0); err == nil ||
		!strings.Contains(err.Error(), "首版只整理普通文件") {
		t.Fatalf("目录源 = %v", err)
	}

	// 源不存在且不是 Move，直接报检查失败。
	missing := Operation{Source: filepath.Join(sourceRoot, "missing"), Target: filepath.Join(targetRoot, "out"), Mode: Copy}
	if err := executeOperation(context.Background(), boundary, missing, "id", 0); err == nil ||
		!strings.Contains(err.Error(), "检查源文件") {
		t.Fatalf("缺源文件 = %v", err)
	}

	// 源越过下载根目录。
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	size, digest := testFingerprint([]byte("x"))
	outsideOperation := Operation{
		Source: outside, Target: filepath.Join(targetRoot, "out"), Mode: Copy,
		ExpectedSize: size, ExpectedSHA256: digest,
	}
	if err := executeOperation(context.Background(), boundary, outsideOperation, "id", 0); err == nil ||
		!strings.Contains(err.Error(), "源路径越过下载根目录") {
		t.Fatalf("越界源 = %v", err)
	}

	// 源是软链接。
	source := filepath.Join(sourceRoot, "source")
	if err := os.WriteFile(source, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(sourceRoot, "link")
	createFileSymlinkOrSkip(t, source, link)
	linkOperation := Operation{Source: link, Target: filepath.Join(targetRoot, "out"), Mode: Copy}
	if err := executeOperation(context.Background(), boundary, linkOperation, "id", 0); err == nil ||
		!strings.Contains(err.Error(), "首版只整理普通文件") {
		t.Fatalf("软链接源 = %v", err)
	}

	// 源指纹不匹配。
	payloadSize, payloadDigest := testFingerprint([]byte("payload"))
	mismatched := Operation{
		Source: source, Target: filepath.Join(targetRoot, "out"), Mode: Copy,
		ExpectedSize: payloadSize, ExpectedSHA256: strings.Repeat("0", 64),
	}
	if err := executeOperation(context.Background(), boundary, mismatched, "id", 0); err == nil ||
		!strings.Contains(err.Error(), "源文件指纹不匹配") {
		t.Fatalf("指纹不匹配 = %v", err)
	}

	// 目标已存在但内容不同。
	target := filepath.Join(targetRoot, "existing")
	if err := os.WriteFile(target, []byte("other"), 0o644); err != nil {
		t.Fatal(err)
	}
	matchingSource := Operation{
		Source: source, Target: target, Mode: Copy,
		ExpectedSize: payloadSize, ExpectedSHA256: payloadDigest,
	}
	if err := executeOperation(context.Background(), boundary, matchingSource, "id", 0); err == nil ||
		!strings.Contains(err.Error(), "目标已存在且指纹不匹配") {
		t.Fatalf("目标冲突 = %v", err)
	}

	// 源在复制途中消失：用已删除的源构造 Copy，仍然走检查源文件分支。
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	if err := executeOperation(context.Background(), boundary, matchingSource, "id", 0); err == nil ||
		!strings.Contains(err.Error(), "检查源文件") {
		t.Fatalf("删除后源 = %v", err)
	}
}

func TestRemoveVerifiedMoveSourceBranches(t *testing.T) {
	targetRoot := t.TempDir()
	sourceRoot := t.TempDir()
	boundary := executionBoundary{targetRoot: targetRoot, sourceRoot: sourceRoot, enforceSourceRoot: true}

	content := []byte("move-payload")
	size, digest := testFingerprint(content)
	makeSource := func(t *testing.T, name string, payload []byte) string {
		t.Helper()
		path := filepath.Join(sourceRoot, name)
		if err := os.WriteFile(path, payload, 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	makeTarget := func(t *testing.T, name string, payload []byte) string {
		t.Helper()
		path := filepath.Join(targetRoot, name)
		if err := os.WriteFile(path, payload, 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}

	// 源越过下载根目录。
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, content, 0o644); err != nil {
		t.Fatal(err)
	}
	operation := Operation{
		Source: outside, Target: makeTarget(t, "t1", content), Mode: Move,
		ExpectedSize: size, ExpectedSHA256: digest,
	}
	if err := removeVerifiedMoveSource(context.Background(), boundary, operation); err == nil ||
		!strings.Contains(err.Error(), "源路径越过下载根目录") {
		t.Fatalf("越界源 = %v", err)
	}

	// 目标越过整理根目录。
	operation = Operation{
		Source: makeSource(t, "s2", content), Target: filepath.Join(t.TempDir(), "t2"), Mode: Move,
		ExpectedSize: size, ExpectedSHA256: digest,
	}
	if err := removeVerifiedMoveSource(context.Background(), boundary, operation); err == nil ||
		!strings.Contains(err.Error(), "目标父目录越过整理根目录") {
		t.Fatalf("越界目标 = %v", err)
	}

	// 源变成目录。
	directorySource := filepath.Join(sourceRoot, "dir-source")
	if err := os.Mkdir(directorySource, 0o755); err != nil {
		t.Fatal(err)
	}
	operation = Operation{
		Source: directorySource, Target: makeTarget(t, "t3", content), Mode: Move,
		ExpectedSize: size, ExpectedSHA256: digest,
	}
	if err := removeVerifiedMoveSource(context.Background(), boundary, operation); err == nil ||
		!strings.Contains(err.Error(), "删除前源文件类型已改变") {
		t.Fatalf("目录源 = %v", err)
	}

	// 源指纹不匹配。
	operation = Operation{
		Source: makeSource(t, "s4", content), Target: makeTarget(t, "t4", content), Mode: Move,
		ExpectedSize: size, ExpectedSHA256: strings.Repeat("0", 64),
	}
	if err := removeVerifiedMoveSource(context.Background(), boundary, operation); err == nil ||
		!strings.Contains(err.Error(), "删除前源文件指纹已改变") {
		t.Fatalf("源指纹 = %v", err)
	}

	// 目标指纹不匹配。
	operation = Operation{
		Source: makeSource(t, "s5", content), Target: makeTarget(t, "t5", []byte("different")), Mode: Move,
		ExpectedSize: size, ExpectedSHA256: digest,
	}
	if err := removeVerifiedMoveSource(context.Background(), boundary, operation); err == nil ||
		!strings.Contains(err.Error(), "删除前目标文件指纹已改变") {
		t.Fatalf("目标指纹 = %v", err)
	}

	// 源已经不存在：只要目标指纹一致就算完成。
	missingSource := filepath.Join(sourceRoot, "s6-missing")
	operation = Operation{
		Source: missingSource, Target: makeTarget(t, "t6", content), Mode: Move,
		ExpectedSize: size, ExpectedSHA256: digest,
	}
	if err := removeVerifiedMoveSource(context.Background(), boundary, operation); err != nil {
		t.Fatalf("源已删除 = %v", err)
	}

	// 正常删除源文件。
	successSource := makeSource(t, "s7", content)
	operation = Operation{
		Source: successSource, Target: makeTarget(t, "t7", content), Mode: Move,
		ExpectedSize: size, ExpectedSHA256: digest,
	}
	if err := removeVerifiedMoveSource(context.Background(), boundary, operation); err != nil {
		t.Fatalf("正常删除 = %v", err)
	}
	if _, err := os.Lstat(successSource); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("源文件未删除")
	}
	if payload, err := os.ReadFile(operation.Target); err != nil || string(payload) != string(content) {
		t.Fatalf("目标文件 = %q %v", payload, err)
	}
}

func TestVerifyPlannedSymlinkMismatch(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source")
	if err := os.WriteFile(source, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(dir, "other")
	if err := os.WriteFile(other, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}

	wrongLink := filepath.Join(dir, "wrong-link")
	createFileSymlinkOrSkip(t, other, wrongLink)
	size, digest := testFingerprint([]byte("payload"))
	operation := Operation{
		Source: source, Target: wrongLink, Mode: Symlink,
		ExpectedSize: size, ExpectedSHA256: digest,
	}
	if err := verifyPlannedTarget(context.Background(), operation); err == nil ||
		!strings.Contains(err.Error(), "目标软链接未解析到计划源文件") {
		t.Fatalf("指向其它文件 = %v", err)
	}

	danglingLink := filepath.Join(dir, "dangling")
	createFileSymlinkOrSkip(t, filepath.Join(dir, "does-not-exist"), danglingLink)
	operation = Operation{Source: source, Target: danglingLink, Mode: Symlink}
	if err := verifyPlannedTarget(context.Background(), operation); err == nil ||
		!strings.Contains(err.Error(), "解析目标软链接失败") {
		t.Fatalf("悬空链接 = %v", err)
	}
}

func TestValidateExistingDirectoryAncestorsBranches(t *testing.T) {
	dir := t.TempDir()
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := validateExistingDirectoryAncestors(canceled, dir, "源"); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消校验 = %v", err)
	}

	// 祖先是普通文件。
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := validateExistingDirectoryAncestors(context.Background(), file, "目标"); err == nil ||
		!strings.Contains(err.Error(), "目标祖先不是目录") {
		t.Fatalf("文件祖先 = %v", err)
	}

	// 祖先不存在时视为通过。
	if err := validateExistingDirectoryAncestors(context.Background(), filepath.Join(dir, "new", "nested"), "源"); err != nil {
		t.Fatalf("缺失祖先 = %v", err)
	}
}

func TestReadFileFingerprintSizeChanges(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file")
	content := []byte("0123456789")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	// 取完 stat 后截断：声明的大小与实际不符。
	if err := os.Truncate(path, 4); err != nil {
		t.Fatal(err)
	}
	size, _ := testFingerprint(content)
	if _, _, err := readFileFingerprint(context.Background(), path, &size, info); err == nil ||
		!strings.Contains(err.Error(), "文件大小为 4") {
		t.Fatalf("期望大小校验 = %v", err)
	}
	// 打开前文件已被替换：inode 变化。
	replacement := filepath.Join(dir, "replacement")
	if err := os.WriteFile(replacement, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	staleInfo, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readFileFingerprint(context.Background(), path, nil, staleInfo); err == nil ||
		!strings.Contains(err.Error(), "文件在打开前已被替换") {
		t.Fatalf("替换检测 = %v", err)
	}
	// 目录不是普通文件。
	if _, _, err := readFileFingerprint(context.Background(), dir, nil, nil); err == nil ||
		!strings.Contains(err.Error(), "文件不是普通文件") {
		t.Fatalf("目录指纹 = %v", err)
	}
	if _, _, err := readFileFingerprint(nil, path, nil, nil); err == nil {
		t.Fatal("nil 上下文应报错")
	}
}

func TestExecuteHardlinkFallsBackToCopy(t *testing.T) {
	otherDevice := "/dev/shm"
	if info, err := os.Stat(otherDevice); err != nil || !info.IsDir() {
		t.Skip("/dev/shm 不可用，跳过跨设备硬链接降级用例")
	}
	targetRoot, err := os.MkdirTemp(otherDevice, "organizer-")
	if err != nil {
		t.Skipf("无法在 /dev/shm 创建目录: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(targetRoot) })
	sourceRoot := t.TempDir()
	sourceInfo, err := os.Stat(sourceRoot)
	if err != nil {
		t.Fatal(err)
	}
	targetInfo, err := os.Stat(targetRoot)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(sourceInfo, targetInfo) {
		t.Skip("源与目标在同一文件系统，无法触发 EXDEV")
	}

	content := []byte("cross-device")
	source := filepath.Join(sourceRoot, "source.mkv")
	if err := os.WriteFile(source, content, 0o644); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(targetRoot, "library", "target.mkv")
	plan := mustTestPlan(t, targetRoot, source, target, Hardlink, content)
	if err := Execute(context.Background(), plan); err != nil {
		t.Fatalf("跨设备硬链接应降级为复制: %v", err)
	}
	if payload, err := os.ReadFile(target); err != nil || string(payload) != string(content) {
		t.Fatalf("目标内容 = %q %v", payload, err)
	}
	// 降级复制不会删除源文件。
	if _, err := os.Lstat(source); err != nil {
		t.Fatal("源文件不应被删除")
	}
}
