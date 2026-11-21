package storage

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	alipanUploadChunkSize = 10 << 20 // 10 MiB per part
	u115PreHashSize       = 128 << 20
)

// FileWriter 是可选能力：将文件内容流式写入/上传到指定存储路径（用于跨存储整理）。
type FileWriter interface {
	Write(ctx context.Context, storageName, path string, reader io.Reader, size int64) error
}

// Write 将内容流原子写入受限本地根目录内的目标路径（自动创建父目录）。
func (p *LocalProvider) Write(ctx context.Context, storageName, requested string, reader io.Reader, _ int64) error {
	if reader == nil {
		return ErrInvalidRequest
	}
	root, err := p.root(storageName)
	if err != nil {
		return err
	}
	if ctx == nil {
		return ErrInvalidRequest
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	target, err := resolvePath(root, requested)
	if err != nil {
		return err
	}
	parent := filepath.Dir(target)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return ErrOperationFailed
	}
	tmp, err := os.CreateTemp(parent, ".mp-upload-*.tmp")
	if err != nil {
		return ErrOperationFailed
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := io.Copy(tmp, reader); err != nil {
		_ = tmp.Close()
		return ErrOperationFailed
	}
	if err := tmp.Close(); err != nil {
		return ErrOperationFailed
	}
	if err := os.Rename(tmpPath, target); err != nil {
		return ErrOperationFailed
	}
	cleanup = false
	return nil
}

// Write 将内容流上传至 SMB 共享路径（自动创建缺失父目录）。
func (p *SMBProvider) Write(ctx context.Context, storageName, requested string, reader io.Reader, _ int64) error {
	if reader == nil {
		return ErrInvalidRequest
	}
	share, cfg, err := p.openShare(ctx, storageName)
	if err != nil {
		return err
	}
	defer func() { _ = share.Close() }()

	rel, displayPath, err := resolveSMBRelativePath(cfg.Root, requested)
	if err != nil {
		return err
	}
	if displayPath == "/" || rel == "." {
		return ErrInvalidRequest
	}
	if parent := path.Dir(rel); parent != "." && parent != "/" && parent != "" {
		if err := share.MkdirAll(parent, 0o755); err != nil {
			return ErrOperationFailed
		}
	}
	file, err := share.Create(rel)
	if err != nil {
		return ErrOperationFailed
	}
	if _, err := io.Copy(file, reader); err != nil {
		_ = file.Close()
		return ErrOperationFailed
	}
	if err := file.Close(); err != nil {
		return ErrOperationFailed
	}
	return nil
}

// Open 通过 Alist `/api/fs/get` 获取直链并打开只读流。
func (p *AlistProvider) Open(ctx context.Context, storageName, requested string) (io.ReadCloser, string, error) {
	if err := p.require(ctx, storageName); err != nil {
		return nil, "", err
	}
	if err := p.ensureToken(ctx); err != nil {
		return nil, "", err
	}
	var info struct {
		Name   string `json:"name"`
		IsDir  bool   `json:"is_dir"`
		RawURL string `json:"raw_url"`
	}
	if err := p.request(ctx, "/api/fs/get", map[string]any{
		"path": p.remotePath(requested), "password": "",
	}, &info); err != nil {
		return nil, "", err
	}
	if info.IsDir || strings.TrimSpace(info.RawURL) == "" {
		return nil, "", ErrOperationFailed
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSpace(info.RawURL), nil)
	if err != nil {
		return nil, "", ErrOperationFailed
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, "", ErrOperationFailed
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_ = resp.Body.Close()
		return nil, "", ErrOperationFailed
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = mime.TypeByExtension(strings.ToLower(path.Ext(info.Name)))
	}
	if ct == "" {
		ct = "application/octet-stream"
	}
	return resp.Body, ct, nil
}

// Write 通过 Alist `/api/fs/put` 流式上传文件到远端存储。
func (p *AlistProvider) Write(ctx context.Context, storageName, requested string, reader io.Reader, size int64) error {
	if reader == nil {
		return ErrInvalidRequest
	}
	if err := p.require(ctx, storageName); err != nil {
		return err
	}
	if err := p.ensureToken(ctx); err != nil {
		return err
	}
	remote := p.remotePath(requested)
	if remote == "/" {
		return ErrInvalidRequest
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, p.config.URL+"/api/fs/put", reader)
	if err != nil {
		return ErrInvalidRequest
	}
	req.Header.Set("File-Path", url.PathEscape(remote))
	req.Header.Set("Content-Type", "application/octet-stream")
	if size >= 0 {
		req.ContentLength = size
	}
	if token := p.loadToken(); token != "" {
		req.Header.Set("Authorization", token)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return ErrOperationFailed
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, alistMaxBody))
	if err != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ErrOperationFailed
	}
	var env alistResponse
	if json.Unmarshal(raw, &env) == nil && env.Code != 0 && env.Code != 200 {
		return ErrOperationFailed
	}
	return nil
}

// ensureFolder 逐级解析或创建阿里云盘目标目录并返回目录 file_id。
func (p *AlipanProvider) ensureFolder(ctx context.Context, accessToken, driveID, dirPath string) (string, error) {
	if dirPath == "/" || dirPath == "." || dirPath == "" {
		return "root", nil
	}
	if existing, err := p.resolveByPath(ctx, accessToken, driveID, dirPath); err == nil && existing.FileID != "" {
		return existing.FileID, nil
	}
	p.mu.Lock()
	baseURL := p.config.BaseURL
	p.mu.Unlock()

	parentID := "root"
	current := ""
	for _, seg := range strings.Split(strings.Trim(dirPath, "/"), "/") {
		if seg == "" {
			continue
		}
		current += "/" + seg
		if existing, err := p.resolveByPath(ctx, accessToken, driveID, current); err == nil && existing.FileID != "" {
			parentID = existing.FileID
			continue
		}
		var created struct {
			FileID  string `json:"file_id"`
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		if err := p.doJSON(ctx, http.MethodPost, baseURL+"/adrive/v1.0/openFile/create", accessToken, map[string]any{
			"drive_id":       driveID,
			"parent_file_id": parentID,
			"name":           seg,
			"type":           "folder",
		}, &created); err != nil || created.Code != "" || created.FileID == "" {
			return "", ErrOperationFailed
		}
		parentID = created.FileID
	}
	return parentID, nil
}

// Write 通过阿里云盘 Open API 分片上传文件（支持自动建目录与秒传响应）。
func (p *AlipanProvider) Write(ctx context.Context, storageName, requested string, reader io.Reader, _ int64) error {
	if reader == nil {
		return ErrInvalidRequest
	}
	if err := p.require(ctx, storageName); err != nil {
		return err
	}
	accessToken, driveID, err := p.ensureAccessToken(ctx)
	if err != nil || driveID == "" {
		return ErrOperationFailed
	}
	cleanPath, err := normalizeVirtualPath(requested)
	if err != nil {
		return err
	}
	if cleanPath == "/" {
		return ErrInvalidRequest
	}
	parentID, err := p.ensureFolder(ctx, accessToken, driveID, path.Dir(cleanPath))
	if err != nil {
		return err
	}

	// 将流落入临时文件以计算精确大小、前 1KB SHA1 并支持可靠分片上传。
	tmpFile, totalSize, preHash, err := spoolForUpload(reader, 1024)
	if err != nil {
		return ErrOperationFailed
	}
	defer func() {
		_ = tmpFile.Close()
		_ = os.Remove(tmpFile.Name())
	}()

	numParts := int((totalSize + alipanUploadChunkSize - 1) / alipanUploadChunkSize)
	if numParts == 0 {
		numParts = 1
	}
	partSpecs := make([]map[string]int, numParts)
	for i := 0; i < numParts; i++ {
		partSpecs[i] = map[string]int{"part_number": i + 1}
	}

	p.mu.Lock()
	baseURL := p.config.BaseURL
	p.mu.Unlock()

	var createResp struct {
		FileID       string `json:"file_id"`
		UploadID     string `json:"upload_id"`
		RapidUpload  bool   `json:"rapid_upload"`
		Exist        bool   `json:"exist"`
		PartInfoList []struct {
			PartNumber int    `json:"part_number"`
			UploadURL  string `json:"upload_url"`
		} `json:"part_info_list"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := p.doJSON(ctx, http.MethodPost, baseURL+"/adrive/v1.0/openFile/create", accessToken, map[string]any{
		"drive_id":        driveID,
		"parent_file_id":  parentID,
		"name":            path.Base(cleanPath),
		"type":            "file",
		"check_name_mode": "auto_rename",
		"size":            totalSize,
		"pre_hash":        preHash,
		"part_info_list":  partSpecs,
	}, &createResp); err != nil || createResp.Code != "" {
		return ErrOperationFailed
	}
	if createResp.RapidUpload || createResp.Exist {
		return nil
	}
	if createResp.FileID == "" || createResp.UploadID == "" {
		return ErrOperationFailed
	}

	for _, part := range createResp.PartInfoList {
		if err := ctx.Err(); err != nil {
			return err
		}
		offset := int64(part.PartNumber-1) * alipanUploadChunkSize
		remaining := totalSize - offset
		if remaining < 0 {
			remaining = 0
		}
		chunkLen := remaining
		if chunkLen > alipanUploadChunkSize {
			chunkLen = alipanUploadChunkSize
		}
		chunk := make([]byte, chunkLen)
		if chunkLen > 0 {
			if _, err := tmpFile.ReadAt(chunk, offset); err != nil && err != io.EOF {
				return ErrOperationFailed
			}
		}
		putReq, err := http.NewRequestWithContext(ctx, http.MethodPut, part.UploadURL, bytes.NewReader(chunk))
		if err != nil {
			return ErrOperationFailed
		}
		putReq.ContentLength = int64(len(chunk))
		putResp, err := p.client.Do(putReq)
		if err != nil {
			return ErrOperationFailed
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(putResp.Body, 4096))
		_ = putResp.Body.Close()
		if putResp.StatusCode < 200 || putResp.StatusCode >= 300 {
			return ErrOperationFailed
		}
	}

	var completeResp struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := p.doJSON(ctx, http.MethodPost, baseURL+"/adrive/v1.0/openFile/complete", accessToken, map[string]any{
		"drive_id":  driveID,
		"file_id":   createResp.FileID,
		"upload_id": createResp.UploadID,
	}, &completeResp); err != nil || completeResp.Code != "" {
		return ErrOperationFailed
	}
	return nil
}

// ensureFolder 逐级解析或创建 115 网盘目标目录并返回目录 cid。
func (p *U115Provider) ensureFolder(ctx context.Context, accessToken, dirPath string) (string, error) {
	if dirPath == "/" || dirPath == "." || dirPath == "" {
		return "0", nil
	}
	if existing, err := p.resolveByPath(ctx, accessToken, dirPath); err == nil {
		if cid := jsonRawToString(existing.FileID); cid != "" {
			return cid, nil
		}
	}
	p.mu.Lock()
	baseURL := p.config.BaseURL
	p.mu.Unlock()

	parentID := "0"
	current := ""
	for _, seg := range strings.Split(strings.Trim(dirPath, "/"), "/") {
		if seg == "" {
			continue
		}
		current += "/" + seg
		if existing, err := p.resolveByPath(ctx, accessToken, current); err == nil {
			if cid := jsonRawToString(existing.FileID); cid != "" {
				parentID = cid
				continue
			}
		}
		form := url.Values{}
		form.Set("pid", parentID)
		form.Set("file_name", seg)
		var resp struct {
			State bool `json:"state"`
			Code  int  `json:"code"`
			Data  struct {
				FileID json.RawMessage `json:"file_id"`
			} `json:"data"`
		}
		if err := p.doForm(ctx, http.MethodPost, baseURL+"/open/folder/add", accessToken, form, &resp); err != nil {
			return "", ErrOperationFailed
		}
		if cid := jsonRawToString(resp.Data.FileID); cid != "" {
			parentID = cid
			continue
		}
		if existing, err := p.resolveByPath(ctx, accessToken, current); err == nil {
			if cid := jsonRawToString(existing.FileID); cid != "" {
				parentID = cid
				continue
			}
		}
		return "", ErrOperationFailed
	}
	return parentID, nil
}

// Write 通过 115 Open API `/open/upload/init` 发起秒传或直传上传。
func (p *U115Provider) Write(ctx context.Context, storageName, requested string, reader io.Reader, _ int64) error {
	if reader == nil {
		return ErrInvalidRequest
	}
	if err := p.require(ctx, storageName); err != nil {
		return err
	}
	accessToken, err := p.ensureAccessToken(ctx)
	if err != nil {
		return ErrOperationFailed
	}
	cleanPath, err := normalizeVirtualPath(requested)
	if err != nil {
		return err
	}
	if cleanPath == "/" {
		return ErrInvalidRequest
	}
	targetCID, err := p.ensureFolder(ctx, accessToken, path.Dir(cleanPath))
	if err != nil {
		return err
	}

	tmpFile, totalSize, preSHA1, fullSHA1, err := spoolForU115Upload(reader)
	if err != nil {
		return ErrOperationFailed
	}
	defer func() {
		_ = tmpFile.Close()
		_ = os.Remove(tmpFile.Name())
	}()

	p.mu.Lock()
	baseURL := p.config.BaseURL
	p.mu.Unlock()

	form := url.Values{}
	form.Set("file_name", path.Base(cleanPath))
	form.Set("file_size", strconv.FormatInt(totalSize, 10))
	form.Set("target", "U_1_"+targetCID)
	form.Set("fileid", fullSHA1)
	form.Set("preid", preSHA1)

	var initResp struct {
		State bool `json:"state"`
		Code  int  `json:"code"`
		Data  struct {
			Status    int    `json:"status"`
			UploadURL string `json:"upload_url"`
		} `json:"data"`
	}
	if err := p.doForm(ctx, http.MethodPost, baseURL+"/open/upload/init", accessToken, form, &initResp); err != nil {
		return ErrOperationFailed
	}
	if !initResp.State {
		return ErrOperationFailed
	}
	// status == 2 表示 115 秒传直接命中完成。
	if initResp.Data.Status == 2 {
		return nil
	}
	if strings.TrimSpace(initResp.Data.UploadURL) != "" {
		if _, err := tmpFile.Seek(0, io.SeekStart); err != nil {
			return ErrOperationFailed
		}
		putReq, err := http.NewRequestWithContext(ctx, http.MethodPut, strings.TrimSpace(initResp.Data.UploadURL), tmpFile)
		if err != nil {
			return ErrOperationFailed
		}
		putReq.ContentLength = totalSize
		putResp, err := p.client.Do(putReq)
		if err != nil {
			return ErrOperationFailed
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(putResp.Body, 4096))
		_ = putResp.Body.Close()
		if putResp.StatusCode < 200 || putResp.StatusCode >= 300 {
			return ErrOperationFailed
		}
		return nil
	}
	return ErrOperationFailed
}

func spoolForUpload(reader io.Reader, preBytes int64) (*os.File, int64, string, error) {
	tmp, err := os.CreateTemp("", "mp-alipan-upload-*")
	if err != nil {
		return nil, 0, "", err
	}
	preHasher := sha1.New()
	var total int64
	buf := make([]byte, 32*1024)
	for {
		n, readErr := reader.Read(buf)
		if n > 0 {
			if total < preBytes {
				remain := preBytes - total
				take := int64(n)
				if take > remain {
					take = remain
				}
				_, _ = preHasher.Write(buf[:take])
			}
			if _, wErr := tmp.Write(buf[:n]); wErr != nil {
				_ = tmp.Close()
				_ = os.Remove(tmp.Name())
				return nil, 0, "", wErr
			}
			total += int64(n)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
			return nil, 0, "", readErr
		}
	}
	return tmp, total, hex.EncodeToString(preHasher.Sum(nil)), nil
}

func spoolForU115Upload(reader io.Reader) (*os.File, int64, string, string, error) {
	tmp, err := os.CreateTemp("", "mp-u115-upload-*")
	if err != nil {
		return nil, 0, "", "", err
	}
	preHasher := sha1.New()
	fullHasher := sha1.New()
	var total int64
	buf := make([]byte, 32*1024)
	for {
		n, readErr := reader.Read(buf)
		if n > 0 {
			_, _ = fullHasher.Write(buf[:n])
			if total < u115PreHashSize {
				remain := u115PreHashSize - total
				take := int64(n)
				if take > remain {
					take = remain
				}
				_, _ = preHasher.Write(buf[:take])
			}
			if _, wErr := tmp.Write(buf[:n]); wErr != nil {
				_ = tmp.Close()
				_ = os.Remove(tmp.Name())
				return nil, 0, "", "", wErr
			}
			total += int64(n)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
			return nil, 0, "", "", readErr
		}
	}
	return tmp, total, hex.EncodeToString(preHasher.Sum(nil)), hex.EncodeToString(fullHasher.Sum(nil)), nil
}

var _ FileWriter = (*LocalProvider)(nil)
var _ FileWriter = (*SMBProvider)(nil)
var _ FileOpener = (*AlistProvider)(nil)
var _ FileWriter = (*AlistProvider)(nil)
var _ FileWriter = (*AlipanProvider)(nil)
var _ FileWriter = (*U115Provider)(nil)
