package admin

// GeodataService 的具体实现（对齐 backend/app/Services/GeodataService.php）。
//
// 链路：面板从白名单源下载 → 落盘（If-Modified-Since 条件请求）→ 入队 geodata_sync
// 指令 → agent 从面板下载。失败安全：下载/校验任何一步失败都保留旧文件。
//
// 下载走 HTTPDoer 接缝；节点分发走 GeodataCommandStore 接缝。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// geodataWhitelist 只允许这两个文件下载/分发（对齐上游 3x-ui geofileAllowlist）。
var geodataWhitelist = map[string]string{
	"geoip.dat":   "https://github.com/Loyalsoldier/v2ray-rules-dat/releases/latest/download/geoip.dat",
	"geosite.dat": "https://github.com/Loyalsoldier/v2ray-rules-dat/releases/latest/download/geosite.dat",
}

// geodataMinBytes 单文件大小下限（防把错误页 / 空内容当成功下载）。
const geodataMinBytes = 100 * 1024

// GeodataCommandStore 是节点指令的写入接缝（node_api_commands）。
type GeodataCommandStore interface {
	// GeodataNodeStatuses 返回 xray 节点及其最近一次 geodata_sync 状态。
	GeodataNodeStatuses(ctx context.Context) ([]GeodataNodeStatus, error)
	// EnqueueGeodataSync 幂等入队：已有在途指令 → skipped。
	EnqueueGeodataSync(ctx context.Context, nodeID uint64, files []GeodataFilePayload) (queued bool, err error)
	// EnabledXrayNodes 返回待分发的节点 ID 列表（按 id 升序）。
	EnabledXrayNodes(ctx context.Context) ([]uint64, error)
	// NodeInFlightGeodataSync 判断该节点是否已有在途 geodata_sync。
	NodeInFlightGeodataSync(ctx context.Context, nodeID uint64) (bool, error)
}

// GeodataFilePayload 是分发指令里的文件描述。
type GeodataFilePayload struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	URL    string `json:"url"`
}

// GeodataHTTPDoer 是下载接缝（测试注入假客户端）。
type GeodataHTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// FileGeodataService 是基于本地目录 + HTTP 的 GeodataService 实现。
type FileGeodataService struct {
	dir    string
	files  BackupFileStore
	cmds   GeodataCommandStore
	client GeodataHTTPDoer
	now    func() time.Time
	logger loggerLike
}

// NewFileGeodataService 构造 geo 数据服务。
func NewFileGeodataService(dir string, files BackupFileStore, cmds GeodataCommandStore, client GeodataHTTPDoer, now func() time.Time, logger loggerLike) *FileGeodataService {
	if now == nil {
		now = time.Now
	}
	if logger == nil {
		logger = log.Default()
	}
	if client == nil {
		client = http.DefaultClient
	}
	return &FileGeodataService{dir: dir, files: files, cmds: cmds, client: client, now: now, logger: logger}
}

func (s *FileGeodataService) filePath(name string) string { return filepath.Join(s.dir, name) }
func (s *FileGeodataService) manifestPath() string        { return filepath.Join(s.dir, "manifest.json") }

type geodataManifestEntry struct {
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"`
	UpdatedAt string `json:"updated_at"`
	CheckedAt string `json:"checked_at"`
}

func (s *FileGeodataService) loadManifest() map[string]geodataManifestEntry {
	out := map[string]geodataManifestEntry{}
	if !s.files.Exists(s.manifestPath()) {
		return out
	}
	raw, err := s.files.ReadFile(s.manifestPath())
	if err != nil {
		return out
	}
	_ = json.Unmarshal(raw, &out)
	return out
}

func (s *FileGeodataService) saveManifest(m map[string]geodataManifestEntry) {
	raw, _ := json.MarshalIndent(m, "", "  ")
	_ = s.files.WriteFile(s.manifestPath(), raw)
}

// Status 对齐 GeodataService::status()。
func (s *FileGeodataService) Status(context.Context) ([]GeodataFile, error) {
	manifest := s.loadManifest()
	var out []GeodataFile
	for _, name := range []string{"geoip.dat", "geosite.dat"} {
		path := s.filePath(name)
		exists := s.files.Exists(path)
		f := GeodataFile{Name: name, Exists: exists}
		if exists {
			f.Size = s.files.Size(path)
			f.SHA256 = s.files.SHA256(path)
		}
		if meta, ok := manifest[name]; ok {
			if meta.UpdatedAt != "" {
				f.UpdatedAt = &meta.UpdatedAt
			}
			if meta.CheckedAt != "" {
				f.CheckedAt = &meta.CheckedAt
			}
		}
		out = append(out, f)
	}
	return out, nil
}

// NodeStatuses 对齐 GeodataService::nodeStatuses()。
func (s *FileGeodataService) NodeStatuses(ctx context.Context) ([]GeodataNodeStatus, error) {
	if s.cmds == nil {
		return nil, nil
	}
	return s.cmds.GeodataNodeStatuses(ctx)
}

// Update 对齐 GeodataService::update()（逐文件条件请求 + 原子替换）。
func (s *FileGeodataService) Update(ctx context.Context) ([]GeodataUpdateResult, error) {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return []GeodataUpdateResult{
			{Name: "geoip.dat", Status: "failed", Message: "创建目录失败: " + s.dir},
			{Name: "geosite.dat", Status: "failed", Message: "创建目录失败: " + s.dir},
		}, nil
	}
	var out []GeodataUpdateResult
	for _, name := range []string{"geoip.dat", "geosite.dat"} {
		out = append(out, s.updateOne(ctx, name, geodataWhitelist[name]))
	}
	return out, nil
}

func (s *FileGeodataService) updateOne(ctx context.Context, name, url string) GeodataUpdateResult {
	target := s.filePath(name)
	headers := map[string]string{"User-Agent": "ControlHub-Geodata/1.0"}
	if s.files.Exists(target) {
		// 条件请求：没变化就 304 跳过
		if st, err := os.Stat(target); err == nil {
			headers["If-Modified-Since"] = st.ModTime().UTC().Format(http.TimeFormat)
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return GeodataUpdateResult{Name: name, Status: "failed", Message: "下载失败: " + err.Error()}
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return GeodataUpdateResult{Name: name, Status: "failed", Message: "下载失败: " + err.Error()}
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified {
		s.touchChecked(name)
		result := GeodataUpdateResult{Name: name, Status: "unchanged"}
		if s.files.Exists(target) {
			result.Size = s.files.Size(target)
			result.SHA256 = s.files.SHA256(target)
		}
		return result
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return GeodataUpdateResult{Name: name, Status: "failed", Message: fmt.Sprintf("上游返回 HTTP %d", resp.StatusCode)}
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return GeodataUpdateResult{Name: name, Status: "failed", Message: "下载失败: " + err.Error()}
	}
	if len(body) < geodataMinBytes {
		return GeodataUpdateResult{
			Name: name, Status: "failed",
			Message: fmt.Sprintf("内容过小（%d 字节），疑似错误响应，已放弃", len(body)),
		}
	}
	sum := sha256.Sum256(body)
	sha := hex.EncodeToString(sum[:])

	// 磁盘上已是同一份 → 不重写（保持 mtime，维持条件请求语义）
	if s.files.Exists(target) && s.files.SHA256(target) == sha {
		s.touchChecked(name)
		return GeodataUpdateResult{Name: name, Status: "unchanged", Size: int64(len(body)), SHA256: sha}
	}

	// 先落临时文件、再原子替换
	tmp := target + ".tmp"
	if err := s.files.WriteFile(tmp, body); err != nil {
		return GeodataUpdateResult{Name: name, Status: "failed", Message: "写入临时文件失败"}
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = s.files.Remove(tmp)
		return GeodataUpdateResult{Name: name, Status: "failed", Message: "替换文件失败"}
	}
	s.recordUpdated(name, int64(len(body)), sha)
	return GeodataUpdateResult{Name: name, Status: "updated", Size: int64(len(body)), SHA256: sha}
}

func (s *FileGeodataService) recordUpdated(name string, size int64, sha string) {
	m := s.loadManifest()
	now := s.now().UTC().Format(time.RFC3339)
	m[name] = geodataManifestEntry{Size: size, SHA256: sha, UpdatedAt: now, CheckedAt: now}
	s.saveManifest(m)
}

func (s *FileGeodataService) touchChecked(name string) {
	m := s.loadManifest()
	e := m[name]
	e.CheckedAt = s.now().UTC().Format(time.RFC3339)
	m[name] = e
	s.saveManifest(m)
}

// DispatchToNodes 对齐 GeodataService::dispatchToNodes()（幂等，防指令风暴）。
func (s *FileGeodataService) DispatchToNodes(ctx context.Context) (int, int, bool, error) {
	var files []GeodataFilePayload
	for _, name := range []string{"geoip.dat", "geosite.dat"} {
		path := s.filePath(name)
		if !s.files.Exists(path) {
			continue
		}
		files = append(files, GeodataFilePayload{
			Name:   name,
			Size:   s.files.Size(path),
			SHA256: s.files.SHA256(path),
			URL:    "/node-bin/geo/" + name,
		})
	}
	if len(files) == 0 {
		return 0, 0, true, nil
	}
	if s.cmds == nil {
		return 0, 0, false, nil
	}
	nodes, err := s.cmds.EnabledXrayNodes(ctx)
	if err != nil {
		return 0, 0, false, err
	}
	queued, skipped := 0, 0
	for _, nodeID := range nodes {
		inFlight, err := s.cmds.NodeInFlightGeodataSync(ctx, nodeID)
		if err != nil {
			return queued, skipped, false, err
		}
		if inFlight {
			skipped++
			continue
		}
		ok, err := s.cmds.EnqueueGeodataSync(ctx, nodeID, files)
		if err != nil {
			return queued, skipped, false, err
		}
		if ok {
			queued++
		} else {
			skipped++
		}
	}
	return queued, skipped, false, nil
}
