// Package torrentmeta 校验有界 BitTorrent metainfo，并从原始 info 字典计算 Hash。
package torrentmeta

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"sort"
	"strings"

	"github.com/zeebo/bencode"
)

// MaxSize 是进程允许在内存中处理的单个 metainfo 硬上限。
const MaxSize = 16 << 20

const (
	maxFileTreeDepth = 64
	maxFileTreeNodes = 100_000
)

var (
	// ErrInvalid 表示 metainfo 编码、布局或 Hash 输入不符合受支持规范。
	ErrInvalid = errors.New("种子 metainfo 无效")
	// ErrTooLarge 表示 metainfo 超出单值内存上限。
	ErrTooLarge = errors.New("种子 metainfo 超过大小上限")
)

// File 描述种子 info 字典中的规范化文件。
type File struct {
	Path   string
	Length int64
}

// Inspection 描述从原始 info 字典得到的 v1/v2 Hash 和文件投影。
type Inspection struct {
	// Name 是 info 字典的顶层 name；对应 V2 `torrent_from_file.name`。
	Name string
	// TotalSize 是所有文件长度之和；对应 V2 `torrent_from_file.total_size`。
	TotalSize   int64
	PrimaryHash string
	InfoHashV1  string
	InfoHashV2  string
	Hybrid      bool
	Files       []File
}

type rawMetainfo struct {
	Info bencode.RawMessage `bencode:"info"`
}

// Inspect 严格校验 metainfo，并直接对原始 info 字典计算 Hash。
// Hybrid 种子以 v1 Hash 作为 PrimaryHash，纯 v2 种子使用 v2 Hash。
func Inspect(body []byte) (Inspection, error) {
	if len(body) == 0 {
		return Inspection{}, ErrInvalid
	}
	if len(body) > MaxSize {
		return Inspection{}, ErrTooLarge
	}

	var metainfo rawMetainfo
	if err := decodeStrict(body, &metainfo); err != nil || len(metainfo.Info) == 0 {
		return Inspection{}, ErrInvalid
	}
	var info map[string]any
	if err := decodeStrict(metainfo.Info, &info); err != nil {
		return Inspection{}, ErrInvalid
	}
	if err := validateCommonInfo(info); err != nil {
		return Inspection{}, err
	}

	v1Present := hasV1Layout(info)
	v2Present, err := hasV2Layout(info)
	if err != nil {
		return Inspection{}, err
	}
	if !v1Present && !v2Present {
		return Inspection{}, ErrInvalid
	}
	if v1Present {
		if err := validateV1Info(info); err != nil {
			return Inspection{}, err
		}
	}
	if v2Present {
		if err := validateV2Info(info); err != nil {
			return Inspection{}, err
		}
	}

	result := Inspection{Hybrid: v1Present && v2Present, Name: info["name"].(string)}
	if v1Present {
		result.Files = projectV1Files(info)
	} else {
		result.Files = projectV2Files(info)
	}
	sort.Slice(result.Files, func(i, j int) bool {
		if result.Files[i].Path == result.Files[j].Path {
			return result.Files[i].Length < result.Files[j].Length
		}
		return result.Files[i].Path < result.Files[j].Path
	})
	for _, file := range result.Files {
		if file.Length < 0 || result.TotalSize > math.MaxInt64-file.Length {
			return Inspection{}, ErrInvalid
		}
		result.TotalSize += file.Length
	}
	if v1Present {
		digest := sha1.Sum(metainfo.Info)
		result.InfoHashV1 = strings.ToUpper(hex.EncodeToString(digest[:]))
		result.PrimaryHash = result.InfoHashV1
	}
	if v2Present {
		digest := sha256.Sum256(metainfo.Info)
		result.InfoHashV2 = strings.ToUpper(hex.EncodeToString(digest[:]))
		if result.PrimaryHash == "" {
			result.PrimaryHash = result.InfoHashV2
		}
	}
	return result, nil
}

func projectV1Files(info map[string]any) []File {
	if length, hasLength := integer(info["length"]); hasLength {
		return []File{{Path: info["name"].(string), Length: length}}
	}

	rawFiles := info["files"].([]any)
	files := make([]File, 0, len(rawFiles))
	for _, rawFile := range rawFiles {
		file := rawFile.(map[string]any)
		rawPath := file["path"].([]any)
		path := make([]string, len(rawPath))
		for index, component := range rawPath {
			path[index] = component.(string)
		}
		length, _ := integer(file["length"])
		files = append(files, File{Path: strings.Join(path, "/"), Length: length})
	}
	return files
}

func projectV2Files(info map[string]any) []File {
	files := make([]File, 0)
	projectV2FileTree(info["file tree"].(map[string]any), nil, &files)
	return files
}

func projectV2FileTree(tree map[string]any, parent []string, files *[]File) {
	for name, rawNode := range tree {
		node := rawNode.(map[string]any)
		path := appendPath(parent, name)
		if rawLeaf, isFile := node[""]; isFile {
			leaf := rawLeaf.(map[string]any)
			length, _ := integer(leaf["length"])
			*files = append(*files, File{Path: strings.Join(path, "/"), Length: length})
			continue
		}
		projectV2FileTree(node, path, files)
	}
}

func appendPath(parent []string, component string) []string {
	path := make([]string, len(parent)+1)
	copy(path, parent)
	path[len(parent)] = component
	return path
}

func decodeStrict(body []byte, target any) error {
	decoder := bencode.NewDecoder(bytes.NewReader(body))
	decoder.SetFailOnUnorderedKeys(true)
	if err := decoder.Decode(target); err != nil || decoder.BytesParsed() != len(body) {
		return ErrInvalid
	}
	return nil
}

func validateCommonInfo(info map[string]any) error {
	name, ok := info["name"].(string)
	if !ok || !validPathComponent(name) {
		return ErrInvalid
	}
	pieceLength, ok := integer(info["piece length"])
	if !ok || pieceLength <= 0 {
		return ErrInvalid
	}
	return nil
}

func hasV1Layout(info map[string]any) bool {
	_, hasPieces := info["pieces"]
	_, hasLength := info["length"]
	_, hasFiles := info["files"]
	return hasPieces || hasLength || hasFiles
}

func hasV2Layout(info map[string]any) (bool, error) {
	versionValue, hasVersion := info["meta version"]
	_, hasTree := info["file tree"]
	if !hasVersion && !hasTree {
		return false, nil
	}
	version, ok := integer(versionValue)
	if !ok || version != 2 || !hasTree {
		return false, ErrInvalid
	}
	return true, nil
}

func validateV1Info(info map[string]any) error {
	pieces, ok := info["pieces"].(string)
	if !ok || len(pieces) == 0 || len(pieces)%sha1.Size != 0 {
		return ErrInvalid
	}
	pieceLength, _ := integer(info["piece length"])
	lengthValue, hasLength := info["length"]
	filesValue, hasFiles := info["files"]
	if hasLength == hasFiles {
		return ErrInvalid
	}

	var totalLength int64
	if hasLength {
		length, ok := integer(lengthValue)
		if !ok || length <= 0 {
			return ErrInvalid
		}
		totalLength = length
	} else {
		files, ok := filesValue.([]any)
		if !ok || len(files) == 0 || len(files) > maxFileTreeNodes {
			return ErrInvalid
		}
		for _, rawFile := range files {
			file, ok := rawFile.(map[string]any)
			if !ok {
				return ErrInvalid
			}
			length, ok := integer(file["length"])
			path, pathOK := file["path"].([]any)
			if !ok || length < 0 || !pathOK || len(path) == 0 || len(path) > maxFileTreeDepth {
				return ErrInvalid
			}
			for _, rawComponent := range path {
				component, ok := rawComponent.(string)
				if !ok || !validPathComponent(component) {
					return ErrInvalid
				}
			}
			if totalLength > math.MaxInt64-length {
				return ErrInvalid
			}
			totalLength += length
		}
		if totalLength <= 0 {
			return ErrInvalid
		}
	}
	expectedPieces := 1 + (totalLength-1)/pieceLength
	if int64(len(pieces)/sha1.Size) != expectedPieces {
		return ErrInvalid
	}
	return nil
}

func validateV2Info(info map[string]any) error {
	pieceLength, _ := integer(info["piece length"])
	if pieceLength < 16<<10 || pieceLength&(pieceLength-1) != 0 {
		return ErrInvalid
	}
	tree, ok := info["file tree"].(map[string]any)
	if !ok || len(tree) == 0 {
		return ErrInvalid
	}
	stats := fileTreeStats{}
	if err := walkFileTree(tree, 0, &stats); err != nil {
		return err
	}
	if stats.files == 0 || stats.totalLength <= 0 {
		return ErrInvalid
	}
	return nil
}

type fileTreeStats struct {
	nodes       int
	files       int
	totalLength int64
}

func walkFileTree(tree map[string]any, depth int, stats *fileTreeStats) error {
	if depth >= maxFileTreeDepth || len(tree) == 0 {
		return ErrInvalid
	}
	for name, rawNode := range tree {
		if name == "" || !validPathComponent(name) {
			return ErrInvalid
		}
		node, ok := rawNode.(map[string]any)
		if !ok || len(node) == 0 {
			return ErrInvalid
		}
		stats.nodes++
		if stats.nodes > maxFileTreeNodes {
			return ErrInvalid
		}
		leafValue, isFile := node[""]
		if isFile {
			if len(node) != 1 {
				return ErrInvalid
			}
			leaf, ok := leafValue.(map[string]any)
			if !ok {
				return ErrInvalid
			}
			length, ok := integer(leaf["length"])
			if !ok || length < 0 || stats.totalLength > math.MaxInt64-length {
				return ErrInvalid
			}
			if root, exists := leaf["pieces root"]; exists {
				encoded, ok := root.(string)
				if !ok || len(encoded) != sha256.Size {
					return ErrInvalid
				}
			}
			stats.files++
			stats.totalLength += length
			continue
		}
		if err := walkFileTree(node, depth+1, stats); err != nil {
			return err
		}
	}
	return nil
}

func validPathComponent(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && value != "." && value != ".." && !strings.ContainsAny(value, "/\\\x00")
}

func integer(value any) (int64, bool) {
	integer, ok := value.(int64)
	return integer, ok
}
