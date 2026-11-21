package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path"
	"strings"
	"testing"
	"time"
)

type fakeSMBInfo struct {
	name    string
	size    int64
	isDir   bool
	modTime time.Time
}

func (f fakeSMBInfo) Name() string       { return f.name }
func (f fakeSMBInfo) Size() int64        { return f.size }
func (f fakeSMBInfo) Mode() os.FileMode  { return 0o644 }
func (f fakeSMBInfo) ModTime() time.Time { return f.modTime }
func (f fakeSMBInfo) IsDir() bool        { return f.isDir }
func (f fakeSMBInfo) Sys() any           { return nil }

type fakeSMBFS struct {
	entries map[string]fakeSMBInfo
	files   map[string][]byte
	total   uint64
	avail   uint64
	closed  bool
}

func newFakeSMBFS() *fakeSMBFS {
	t1 := time.Unix(1700000100, 0)
	t2 := time.Unix(1700000200, 0)
	return &fakeSMBFS{
		entries: map[string]fakeSMBInfo{
			"Movies":                  {name: "Movies", isDir: true, modTime: t1},
			"Movies/Inception.mkv":    {name: "Inception.mkv", size: 2048, isDir: false, modTime: t2},
			"Movies/Interstellar.srt": {name: "Interstellar.srt", size: 128, isDir: false, modTime: t1},
		},
		files: map[string][]byte{
			"Movies/Interstellar.srt": []byte("1\n00:00:01,000 --> 00:00:02,000\nHello\n"),
		},
		total: 100 << 30,
		avail: 40 << 30,
	}
}

func (f *fakeSMBFS) ReadDir(name string) ([]os.FileInfo, error) {
	prefix := strings.Trim(name, "/")
	if prefix == "." {
		prefix = ""
	}
	var out []os.FileInfo
	for p, info := range f.entries {
		dir := path.Dir(p)
		if dir == "." {
			dir = ""
		}
		if dir == prefix {
			out = append(out, info)
		}
	}
	return out, nil
}

func (f *fakeSMBFS) Stat(name string) (os.FileInfo, error) {
	clean := strings.Trim(name, "/")
	info, ok := f.entries[clean]
	if !ok {
		return nil, os.ErrNotExist
	}
	return info, nil
}

func (f *fakeSMBFS) MkdirAll(name string, _ os.FileMode) error {
	clean := strings.Trim(name, "/")
	f.entries[clean] = fakeSMBInfo{name: path.Base(clean), isDir: true, modTime: time.Unix(1700000300, 0)}
	return nil
}

func (f *fakeSMBFS) RemoveAll(name string) error {
	clean := strings.Trim(name, "/")
	if _, ok := f.entries[clean]; !ok {
		return os.ErrNotExist
	}
	delete(f.entries, clean)
	delete(f.files, clean)
	return nil
}

func (f *fakeSMBFS) Rename(oldpath, newpath string) error {
	oldClean := strings.Trim(oldpath, "/")
	newClean := strings.Trim(newpath, "/")
	info, ok := f.entries[oldClean]
	if !ok {
		return os.ErrNotExist
	}
	info.name = path.Base(newClean)
	f.entries[newClean] = info
	delete(f.entries, oldClean)
	if content, ok := f.files[oldClean]; ok {
		f.files[newClean] = content
		delete(f.files, oldClean)
	}
	return nil
}

func (f *fakeSMBFS) Open(name string) (io.ReadCloser, error) {
	clean := strings.Trim(name, "/")
	data, ok := f.files[clean]
	if !ok {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

type fakeSMBWriteFile struct {
	fs   *fakeSMBFS
	path string
	buf  bytes.Buffer
}

func (w *fakeSMBWriteFile) Write(p []byte) (int, error) {
	return w.buf.Write(p)
}

func (w *fakeSMBWriteFile) Close() error {
	data := append([]byte(nil), w.buf.Bytes()...)
	w.fs.files[w.path] = data
	w.fs.entries[w.path] = fakeSMBInfo{
		name:    path.Base(w.path),
		size:    int64(len(data)),
		isDir:   false,
		modTime: time.Now(),
	}
	return nil
}

func (f *fakeSMBFS) Create(name string) (io.WriteCloser, error) {
	clean := strings.Trim(name, "/")
	return &fakeSMBWriteFile{fs: f, path: clean}, nil
}

func (f *fakeSMBFS) Statfs(string) (uint64, uint64, error) {
	return f.total, f.avail, nil
}

func (f *fakeSMBFS) Close() error {
	f.closed = true
	return nil
}

func TestSMBProviderOperationsAndConfig(t *testing.T) {
	fake := newFakeSMBFS()
	var savedConf map[string]any
	provider, err := NewSMBProvider(SMBConfig{
		Host:  "192.168.1.10",
		Share: "media",
		OnConfigChange: func(_ context.Context, conf map[string]any) error {
			savedConf = conf
			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewSMBProvider: %v", err)
	}
	provider.dial = func(context.Context, SMBConfig) (smbFS, error) {
		fake.closed = false
		return fake, nil
	}

	ctx := context.Background()
	items, err := provider.List(ctx, "smb", "/Movies", "name", "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 2 || items[0].Name != "Inception.mkv" || items[1].Name != "Interstellar.srt" {
		t.Fatalf("unexpected items: %+v", items)
	}

	// Keyword glob filter
	filtered, err := provider.List(ctx, "smb", "/Movies", "name", "*.srt")
	if err != nil || len(filtered) != 1 || filtered[0].Name != "Interstellar.srt" {
		t.Fatalf("filtered = %+v, err = %v", filtered, err)
	}

	// Path traversal rejected
	if _, err := provider.List(ctx, "smb", "/Movies/../etc", "name", ""); !errors.Is(err, ErrPathRejected) {
		t.Fatalf("expected ErrPathRejected, got %v", err)
	}

	// Mkdir + Rename + Delete
	ok, err := provider.Mkdir(ctx, "smb", "/Movies", "Action")
	if err != nil || !ok {
		t.Fatalf("Mkdir: ok=%v err=%v", ok, err)
	}
	ok, err = provider.Rename(ctx, "smb", "/Movies/Action", "SciFi", false)
	if err != nil || !ok {
		t.Fatalf("Rename: ok=%v err=%v", ok, err)
	}
	if _, statErr := fake.Stat("Movies/SciFi"); statErr != nil {
		t.Fatalf("expected renamed dir Movies/SciFi: %v", statErr)
	}
	ok, err = provider.Delete(ctx, "smb", "/Movies/SciFi")
	if err != nil || !ok {
		t.Fatalf("Delete: ok=%v err=%v", ok, err)
	}

	// Open file and verify underlying SMB share closes on stream close
	rc, contentType, err := provider.Open(ctx, "smb", "/Movies/Interstellar.srt")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	content, _ := io.ReadAll(rc)
	if !strings.Contains(string(content), "Hello") || contentType == "" {
		t.Fatalf("unexpected open result: content=%q type=%q", string(content), contentType)
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !fake.closed {
		t.Fatalf("expected SMB share to close when file reader closes")
	}

	// Usage + TransType
	usage, err := provider.Usage(ctx, "smb")
	if err != nil || usage.Total != float64(100<<30) || usage.Available != float64(40<<30) {
		t.Fatalf("Usage = %+v, err = %v", usage, err)
	}
	transtype, err := provider.TransType(ctx, "smb")
	if err != nil || transtype["move"] != "移动" || transtype["link"] != "硬链接" {
		t.Fatalf("TransType = %+v, err = %v", transtype, err)
	}

	// SaveConfig + ResetConfig
	if err := provider.SaveConfig(ctx, "smb", map[string]any{
		"host": "10.0.0.2", "port": 1445, "share": "video", "username": "nas",
	}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	if savedConf["host"] != "10.0.0.2" || savedConf["port"] != 1445 || savedConf["share"] != "video" {
		t.Fatalf("unexpected savedConf: %+v", savedConf)
	}
	if err := provider.ResetConfig(ctx, "smb"); err != nil {
		t.Fatalf("ResetConfig: %v", err)
	}
	if _, err := provider.List(ctx, "smb", "/", "name", ""); !errors.Is(err, ErrOperationFailed) {
		t.Fatalf("expected ErrOperationFailed after reset, got %v", err)
	}
}
