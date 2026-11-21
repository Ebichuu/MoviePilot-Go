package httpapi

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
)

// TransferTaskView 描述单条进行中/排队中的整理任务（对齐 V2 `/transfer/queue` 与 `/transfer/now`）。
type TransferTaskView struct {
	ID               string  `json:"id"`
	Source           string  `json:"source"`
	Target           string  `json:"target"`
	Storage          string  `json:"storage"`
	TargetStorage    string  `json:"target_storage"`
	Mode             string  `json:"mode"`
	State            string  `json:"state"` // "running" | "waiting"
	TotalBytes       int64   `json:"total_bytes"`
	TransferredBytes int64   `json:"transferred_bytes"`
	Percent          float64 `json:"percent"`
	Title            string  `json:"title"`
	StartedAt        string  `json:"started_at"`
}

type trackedTransferTask struct {
	id            string
	source        string
	target        string
	storage       string
	targetStorage string
	mode          string
	state         string
	totalBytes    int64
	transferred   int64
	startedAt     time.Time
	cancel        context.CancelFunc
}

// TransferTracker 线程安全地跟踪运行中与排队中的文件整理任务及实时进度。
type TransferTracker struct {
	mu    sync.RWMutex
	seq   atomic.Int64
	tasks map[string]*trackedTransferTask
	order []string
}

// NewTransferTracker 创建实时整理进度跟踪器。
func NewTransferTracker() *TransferTracker {
	return &TransferTracker{
		tasks: make(map[string]*trackedTransferTask),
	}
}

// Start 注册一条运行中的整理任务并返回其唯一 ID。
func (t *TransferTracker) Start(
	source, target, srcStorage, dstStorage, mode string,
	totalBytes int64,
	cancel context.CancelFunc,
) string {
	if t == nil {
		return ""
	}
	id := "tr-" + strconv.FormatInt(t.seq.Add(1), 10)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.tasks == nil {
		t.tasks = make(map[string]*trackedTransferTask)
	}
	t.tasks[id] = &trackedTransferTask{
		id:            id,
		source:        source,
		target:        target,
		storage:       srcStorage,
		targetStorage: dstStorage,
		mode:          mode,
		state:         "running",
		totalBytes:    totalBytes,
		startedAt:     time.Now(),
		cancel:        cancel,
	}
	t.order = append(t.order, id)
	return id
}

// UpdateBytes 更新指定任务已传输字节数。
func (t *TransferTracker) UpdateBytes(id string, transferred int64) {
	if t == nil || id == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if task, ok := t.tasks[id]; ok {
		task.transferred = transferred
	}
}

// Finish 结束并从活动队列中移除指定任务。
func (t *TransferTracker) Finish(id string) {
	if t == nil || id == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.tasks, id)
	filtered := t.order[:0]
	for _, existing := range t.order {
		if existing != id {
			filtered = append(filtered, existing)
		}
	}
	t.order = filtered
}

// Cancel 按任务 ID 或源路径取消并移除任务。
func (t *TransferTracker) Cancel(idOrPath string) bool {
	if t == nil {
		return false
	}
	key := strings.TrimSpace(idOrPath)
	if key == "" {
		return false
	}
	t.mu.Lock()
	var target *trackedTransferTask
	if task, ok := t.tasks[key]; ok {
		target = task
	} else {
		for _, task := range t.tasks {
			if task.source == key || task.target == key {
				target = task
				break
			}
		}
	}
	if target == nil {
		t.mu.Unlock()
		return false
	}
	cancel := target.cancel
	delete(t.tasks, target.id)
	filtered := t.order[:0]
	for _, existing := range t.order {
		if existing != target.id {
			filtered = append(filtered, existing)
		}
	}
	t.order = filtered
	t.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	return true
}

// List 返回所有排队/运行中的任务快照（runningOnly=true 时仅返回运行中任务）。
func (t *TransferTracker) List(runningOnly bool) []TransferTaskView {
	if t == nil {
		return []TransferTaskView{}
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]TransferTaskView, 0, len(t.order))
	for _, id := range t.order {
		task, ok := t.tasks[id]
		if !ok {
			continue
		}
		if runningOnly && task.state != "running" {
			continue
		}
		pct := 0.0
		if task.totalBytes > 0 {
			pct = float64(task.transferred) * 100 / float64(task.totalBytes)
			if pct > 100 {
				pct = 100
			}
		}
		out = append(out, TransferTaskView{
			ID:               task.id,
			Source:           task.source,
			Target:           task.target,
			Storage:          task.storage,
			TargetStorage:    task.targetStorage,
			Mode:             task.mode,
			State:            task.state,
			TotalBytes:       task.totalBytes,
			TransferredBytes: task.transferred,
			Percent:          pct,
			Title:            filepath.Base(task.source),
			StartedAt:        task.startedAt.Format("2006-01-02 15:04:05"),
		})
	}
	return out
}

// Progress 返回当前活动整理任务的汇总进度（对齐 V2 `/system/progress/{process_type}`）。
func (t *TransferTracker) Progress(processType string) (percent float64, text string, active bool) {
	items := t.List(true)
	if len(items) == 0 {
		return 0, "", false
	}
	pt := strings.ToLower(strings.TrimSpace(processType))
	if pt != "" && pt != "filetransfer" && pt != "transfer" && pt != "all" {
		return 0, "", false
	}
	var totalBytes, doneBytes int64
	for _, it := range items {
		totalBytes += it.TotalBytes
		doneBytes += it.TransferredBytes
	}
	if totalBytes > 0 {
		percent = float64(doneBytes) * 100 / float64(totalBytes)
		if percent > 100 {
			percent = 100
		}
	}
	first := items[0]
	if len(items) == 1 {
		text = fmt.Sprintf("正在整理 %s (%s -> %s)", first.Title, first.Storage, first.TargetStorage)
	} else {
		text = fmt.Sprintf("正在整理 %s 等 %d 个文件", first.Title, len(items))
	}
	return percent, text, true
}

// progressReader 包装 io.Reader，在流式拷贝过程中实时向 TransferTracker 上报字节进度。
type progressReader struct {
	reader  io.Reader
	tracker *TransferTracker
	taskID  string
	read    int64
}

func (r *progressReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if n > 0 {
		r.read += int64(n)
		r.tracker.UpdateBytes(r.taskID, r.read)
	}
	return n, err
}

func (d Dependencies) transferQueueList(w http.ResponseWriter, _ *http.Request) {
	if d.TransferTracker == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	writeJSON(w, http.StatusOK, d.TransferTracker.List(false))
}

func (d Dependencies) transferNowList(w http.ResponseWriter, _ *http.Request) {
	if d.TransferTracker == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	writeJSON(w, http.StatusOK, d.TransferTracker.List(true))
}

func (d Dependencies) removeTransferQueueItem(w http.ResponseWriter, r *http.Request) {
	if d.TransferTracker == nil {
		writeJSON(w, http.StatusOK, Response{Success: true})
		return
	}
	key := strings.TrimSpace(r.URL.Query().Get("id"))
	if key == "" {
		key = strings.TrimSpace(r.URL.Query().Get("path"))
	}
	if key == "" {
		key = strings.TrimSpace(r.URL.Query().Get("source"))
	}
	if key != "" {
		_ = d.TransferTracker.Cancel(key)
	}
	writeJSON(w, http.StatusOK, Response{Success: true})
}

func (d Dependencies) systemProgress(w http.ResponseWriter, r *http.Request) {
	processType := strings.TrimSpace(chi.URLParam(r, "process_type"))
	if d.TransferTracker != nil {
		if pct, text, active := d.TransferTracker.Progress(processType); active {
			writeJSON(w, http.StatusOK, Response{
				Success: true,
				Data: map[string]any{
					"value":  pct,
					"text":   text,
					"enable": true,
				},
			})
			return
		}
	}
	writeJSON(w, http.StatusOK, Response{
		Success: false,
		Message: "没有正在执行的任务",
		Data:    map[string]any{},
	})
}
