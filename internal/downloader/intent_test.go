package downloader

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

const fixtureTorrentHash = "0123456789ABCDEF0123456789ABCDEF01234567"

func TestNewIntentBuildsDeterministicSafeIdentity(t *testing.T) {
	base := Candidate{Key: "  opaque-resource:42  ", DownloadURL: "transient-a"}
	first, err := NewIntent("  admin  ", base)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewIntent("admin", Candidate{Key: "opaque-resource:42", DownloadURL: "transient-b"})
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("相同用户和候选必须生成相同意图：first=%#v second=%#v", first, second)
	}
	if first.State != IntentPlanned || first.TorrentHash != "" {
		t.Fatalf("新意图状态错误：%#v", first)
	}
	if !validOpaqueValue(first.CandidateKey, "sha256:", 64) {
		t.Fatalf("关联键格式不安全：%q", first.CandidateKey)
	}
	if !validOpaqueValue(first.Label, "mp-", 32) {
		t.Fatalf("qB 标签格式不安全：%q", first.Label)
	}
	if first.CandidateKey == strings.TrimSpace(base.Key) || strings.Contains(first.Label, strings.TrimSpace(base.Key)) {
		t.Fatal("意图身份暴露了候选原始定位信息")
	}
}

func TestNewIntentIdentityChangesWithOwnerOrCandidate(t *testing.T) {
	tests := []struct {
		name      string
		username  string
		candidate Candidate
	}{
		{name: "不同用户", username: "other", candidate: Candidate{Key: "opaque-resource:42"}},
		{name: "不同候选", username: "admin", candidate: Candidate{Key: "opaque-resource:43"}},
	}

	base, err := NewIntent("admin", Candidate{Key: "opaque-resource:42"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NewIntent(tt.username, tt.candidate)
			if err != nil {
				t.Fatal(err)
			}
			if got.ID == base.ID || got.Label == base.Label {
				t.Fatalf("不同身份产生了相同意图：base=%#v got=%#v", base, got)
			}
		})
	}
}

func TestNewIntentRejectsMissingIdentity(t *testing.T) {
	tests := []struct {
		name      string
		username  string
		candidate Candidate
	}{
		{name: "缺少用户", candidate: Candidate{Key: "opaque-resource:42"}},
		{name: "缺少候选键", username: "admin"},
		{name: "候选键只有空白", username: "admin", candidate: Candidate{Key: " \t\r\n "}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewIntent(tt.username, tt.candidate)
			if err == nil || err.Error() != "下载意图缺少用户或候选关联键" {
				t.Fatalf("NewIntent() error = %v", err)
			}
		})
	}
}

func TestNormalizeTorrentHash(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "v1 小写", input: strings.ToLower(fixtureTorrentHash), want: fixtureTorrentHash},
		{name: "v1 去除空白", input: " \t" + strings.ToLower(fixtureTorrentHash) + "\r\n", want: fixtureTorrentHash},
		{name: "v2", input: strings.Repeat("ab", 32), want: strings.Repeat("AB", 32)},
		{name: "空值", wantErr: true},
		{name: "长度错误", input: strings.Repeat("a", 39), wantErr: true},
		{name: "非十六进制", input: strings.Repeat("z", 40), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeTorrentHash(tt.input)
			if tt.wantErr {
				if err == nil || err.Error() != "种子 Hash 无效" {
					t.Fatalf("NormalizeTorrentHash() error = %v", err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("NormalizeTorrentHash() = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}

func TestIntentTransition(t *testing.T) {
	tests := []struct {
		name    string
		from    IntentState
		to      IntentState
		hash    string
		wantErr bool
	}{
		{name: "计划状态幂等", from: IntentPlanned, to: IntentPlanned},
		{name: "计划进入提交", from: IntentPlanned, to: IntentSubmitting},
		{name: "提交进入下载", from: IntentSubmitting, to: IntentDownloading, hash: strings.ToLower(fixtureTorrentHash)},
		{name: "提交明确失败", from: IntentSubmitting, to: IntentFailed},
		{name: "明确失败重试", from: IntentFailed, to: IntentSubmitting},
		{name: "下载完成", from: IntentDownloading, to: IntentCompleted, hash: fixtureTorrentHash},
		{name: "下载明确失败", from: IntentDownloading, to: IntentFailed, hash: fixtureTorrentHash},
		{name: "完成状态幂等", from: IntentCompleted, to: IntentCompleted, hash: fixtureTorrentHash},
		{name: "计划跳过提交", from: IntentPlanned, to: IntentDownloading, hash: fixtureTorrentHash, wantErr: true},
		{name: "计划直接失败", from: IntentPlanned, to: IntentFailed, wantErr: true},
		{name: "提交跳到完成", from: IntentSubmitting, to: IntentCompleted, hash: fixtureTorrentHash, wantErr: true},
		{name: "下载回退提交", from: IntentDownloading, to: IntentSubmitting, hash: fixtureTorrentHash, wantErr: true},
		{name: "完成回退下载", from: IntentCompleted, to: IntentDownloading, hash: fixtureTorrentHash, wantErr: true},
		{name: "失败回退计划", from: IntentFailed, to: IntentPlanned, wantErr: true},
		{name: "下载缺少 hash", from: IntentSubmitting, to: IntentDownloading, wantErr: true},
		{name: "未知源状态", from: IntentState("unknown"), to: IntentSubmitting, wantErr: true},
		{name: "未知目标状态", from: IntentPlanned, to: IntentState("unknown"), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			intent := Intent{State: tt.from, TorrentHash: tt.hash}
			updated, err := intent.Transition(tt.to)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Transition(%q -> %q) error = nil", tt.from, tt.to)
				}
				if updated != intent {
					t.Fatalf("失败转换修改了意图：before=%#v after=%#v", intent, updated)
				}
				return
			}
			if err != nil || updated.State != tt.to {
				t.Fatalf("Transition(%q -> %q) = %#v, %v", tt.from, tt.to, updated, err)
			}
			if (tt.to == IntentDownloading || tt.to == IntentCompleted) && updated.TorrentHash != fixtureTorrentHash {
				t.Fatalf("状态 Hash 未规范化：%q", updated.TorrentHash)
			}
		})
	}
}

func TestDecideReconciliation(t *testing.T) {
	tests := []struct {
		name    string
		state   IntentState
		found   bool
		want    ReconcileDecision
		wantErr bool
	}{
		{name: "已存在即复用", state: IntentPlanned, found: true, want: ReconcileReuseExisting},
		{name: "计划状态提交新任务", state: IntentPlanned, want: ReconcileSubmitNew},
		{name: "未知提交结果查 hash", state: IntentSubmitting, want: ReconcileCheckHash},
		{name: "明确失败才可重试", state: IntentFailed, want: ReconcileRetryFailed},
		{name: "下载状态不重复添加", state: IntentDownloading, want: ReconcileCheckHash},
		{name: "完成状态不重复添加", state: IntentCompleted, want: ReconcileCheckHash},
		{name: "未知状态", state: IntentState("unknown"), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := DecideReconciliation(tt.state, tt.found)
			if tt.wantErr {
				if err == nil {
					t.Fatal("DecideReconciliation() error = nil")
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("DecideReconciliation() = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}

func TestSubmitReusesExistingByLabel(t *testing.T) {
	candidate := Candidate{Key: "opaque-resource:42"}
	intent := mustIntent(t, candidate)
	client := &scriptedClient{labelResults: []lookupResult{{hash: strings.ToLower(fixtureTorrentHash), found: true}}}

	updated, err := NewSubmitter(client).Submit(context.Background(), intent, candidate)
	if err != nil || updated.State != IntentDownloading || updated.TorrentHash != fixtureTorrentHash {
		t.Fatalf("Submit() = %#v, %v", updated, err)
	}
	if client.addCalls != 0 {
		t.Fatalf("已存在任务仍调用 Add %d 次", client.addCalls)
	}
}

func TestSubmitReusesExistingByHash(t *testing.T) {
	candidate := Candidate{Key: "opaque-resource:42"}
	intent := mustIntent(t, candidate)
	intent, err := intent.WithTorrentHash(strings.ToLower(fixtureTorrentHash))
	if err != nil {
		t.Fatal(err)
	}
	client := &scriptedClient{
		labelResults: []lookupResult{{}},
		hashResults:  []lookupResult{{hash: fixtureTorrentHash, found: true}},
	}

	updated, err := NewSubmitter(client).Submit(context.Background(), intent, candidate)
	if err != nil || updated.State != IntentDownloading || updated.TorrentHash != fixtureTorrentHash {
		t.Fatalf("Submit() = %#v, %v", updated, err)
	}
	if client.addCalls != 0 || client.hashCalls != 1 {
		t.Fatalf("calls: add=%d hash=%d", client.addCalls, client.hashCalls)
	}
}

func TestSubmitReconcilesLostResponse(t *testing.T) {
	candidate := Candidate{Key: "opaque-resource:42"}
	intent := mustIntent(t, candidate)
	client := &scriptedClient{
		labelResults: []lookupResult{{}, {hash: fixtureTorrentHash, found: true}},
		addErrors:    []error{errors.New("opaque upstream failure")},
	}

	updated, err := NewSubmitter(client).Submit(context.Background(), intent, candidate)
	if err != nil || updated.State != IntentDownloading || updated.TorrentHash != fixtureTorrentHash {
		t.Fatalf("Submit() = %#v, %v", updated, err)
	}
	if client.addCalls != 1 {
		t.Fatalf("Add calls = %d, want 1", client.addCalls)
	}
}

func TestSubmitUnknownResultOnlyReconciles(t *testing.T) {
	candidate := Candidate{Key: "opaque-resource:42"}
	intent := mustIntent(t, candidate)
	intent, err := intent.WithTorrentHash(fixtureTorrentHash)
	if err != nil {
		t.Fatal(err)
	}
	client := &scriptedClient{addErrors: []error{errors.New("opaque upstream failure")}}
	submitter := NewSubmitter(client)

	updated, err := submitter.Submit(context.Background(), intent, candidate)
	if !errors.Is(err, ErrReconciliationPending) || updated.State != IntentSubmitting {
		t.Fatalf("首次 Submit() = %#v, %v", updated, err)
	}
	if client.addCalls != 1 || client.hashCalls != 2 {
		t.Fatalf("首次 calls: add=%d hash=%d", client.addCalls, client.hashCalls)
	}

	updatedAgain, err := submitter.Submit(context.Background(), updated, candidate)
	if !errors.Is(err, ErrReconciliationPending) || updatedAgain.State != IntentSubmitting {
		t.Fatalf("重试 Submit() = %#v, %v", updatedAgain, err)
	}
	if client.addCalls != 1 {
		t.Fatalf("未知结果被重复 Add：%d", client.addCalls)
	}
	if client.hashCalls != 3 {
		t.Fatalf("未知结果未继续按 Hash 对账：%d", client.hashCalls)
	}
}

func TestSubmitRetriesOnlyAfterDefinitiveFailure(t *testing.T) {
	candidate := Candidate{Key: "opaque-resource:42"}
	intent := mustIntent(t, candidate)
	client := &scriptedClient{
		labelResults: []lookupResult{{}, {}, {}, {hash: fixtureTorrentHash, found: true}},
		addErrors:    []error{ErrDefinitiveAddFailure, nil},
	}
	submitter := NewSubmitter(client)

	failed, err := submitter.Submit(context.Background(), intent, candidate)
	if !errors.Is(err, ErrDefinitiveAddFailure) || failed.State != IntentFailed {
		t.Fatalf("明确失败 Submit() = %#v, %v", failed, err)
	}
	if client.addCalls != 1 {
		t.Fatalf("Add calls = %d, want 1", client.addCalls)
	}

	updated, err := submitter.Submit(context.Background(), failed, candidate)
	if err != nil || updated.State != IntentDownloading || updated.TorrentHash != fixtureTorrentHash {
		t.Fatalf("失败重试 Submit() = %#v, %v", updated, err)
	}
	if client.addCalls != 2 {
		t.Fatalf("明确失败未执行一次重试：%d", client.addCalls)
	}
}

func TestSubmitRejectsCandidateMismatch(t *testing.T) {
	intent := mustIntent(t, Candidate{Key: "opaque-resource:42"})
	client := &scriptedClient{}
	_, err := NewSubmitter(client).Submit(context.Background(), intent, Candidate{Key: "opaque-resource:43"})
	if err == nil || err.Error() != "下载候选与意图不匹配" {
		t.Fatalf("Submit() error = %v", err)
	}
	if client.labelCalls != 0 || client.hashCalls != 0 || client.addCalls != 0 {
		t.Fatalf("不匹配候选触发了下载器调用：%#v", client)
	}
}

func FuzzCandidateAssociationKey(f *testing.F) {
	f.Add("opaque-resource:1")
	f.Add("  opaque-resource:2  ")
	f.Fuzz(func(t *testing.T, raw string) {
		key, err := CandidateAssociationKey(Candidate{Key: raw, DownloadURL: "transient"})
		if strings.TrimSpace(raw) == "" {
			if err == nil {
				t.Fatal("空候选键未被拒绝")
			}
			return
		}
		if err != nil {
			t.Fatalf("CandidateAssociationKey() error = %v", err)
		}
		if !validOpaqueValue(key, "sha256:", 64) {
			t.Fatalf("关联键格式无效：%q", key)
		}
		again, err := CandidateAssociationKey(Candidate{Key: raw, DownloadURL: "different-transient"})
		if err != nil || again != key {
			t.Fatalf("关联键不确定：first=%q second=%q err=%v", key, again, err)
		}
	})
}

func FuzzNormalizeTorrentHash(f *testing.F) {
	f.Add(strings.ToLower(fixtureTorrentHash))
	f.Add(strings.Repeat("ab", 32))
	f.Add("invalid")
	f.Fuzz(func(t *testing.T, raw string) {
		normalized, err := NormalizeTorrentHash(raw)
		if err != nil {
			return
		}
		if len(normalized) != 40 && len(normalized) != 64 {
			t.Fatalf("规范化 Hash 长度错误：%d", len(normalized))
		}
		if normalized != strings.ToUpper(normalized) {
			t.Fatalf("规范化 Hash 不是大写：%q", normalized)
		}
		if _, err := hex.DecodeString(normalized); err != nil {
			t.Fatalf("规范化 Hash 不是十六进制：%v", err)
		}
		again, err := NormalizeTorrentHash(normalized)
		if err != nil || again != normalized {
			t.Fatalf("Hash 规范化不幂等：first=%q second=%q err=%v", normalized, again, err)
		}
	})
}

type lookupResult struct {
	hash  string
	found bool
	err   error
}

type scriptedClient struct {
	labelResults []lookupResult
	hashResults  []lookupResult
	addErrors    []error
	labelCalls   int
	hashCalls    int
	addCalls     int
}

func (c *scriptedClient) FindByLabel(context.Context, string) (string, bool, error) {
	c.labelCalls++
	return popLookup(&c.labelResults)
}

func (c *scriptedClient) FindByHash(context.Context, string) (string, bool, error) {
	c.hashCalls++
	return popLookup(&c.hashResults)
}

func (c *scriptedClient) Add(context.Context, Candidate, string) error {
	c.addCalls++
	if len(c.addErrors) == 0 {
		return nil
	}
	err := c.addErrors[0]
	c.addErrors = c.addErrors[1:]
	return err
}

func popLookup(results *[]lookupResult) (string, bool, error) {
	if len(*results) == 0 {
		return "", false, nil
	}
	result := (*results)[0]
	*results = (*results)[1:]
	return result.hash, result.found, result.err
}

func mustIntent(t *testing.T, candidate Candidate) Intent {
	t.Helper()
	intent, err := NewIntent("admin", candidate)
	if err != nil {
		t.Fatal(err)
	}
	return intent
}

func validOpaqueValue(value, prefix string, hexLength int) bool {
	if !strings.HasPrefix(value, prefix) || len(value) != len(prefix)+hexLength {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, prefix))
	return err == nil
}
