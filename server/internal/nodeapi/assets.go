package nodeapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"
)

// 节点资产分发：逐行对齐 backend/app/Http/Controllers/Api/NodeAssetController.php。
//
// 全部公开端点（节点安装/注册前无凭据可鉴权）：
//   GET /node-bin/manifest.json         版本+架构清单（sha256 在此下发）
//   GET /node-bin/agent/{arch}          节点 agent 二进制（白名单 + sha256 复验）
//   GET /node-bin/{version}/{arch}.zip  内核二进制（白名单 + sha256 复验）
//   GET /node-bin/geo/{file}            geo 数据（geoip.dat / geosite.dat）
//   GET /node-install.sh                节点安装脚本
//   GET /node-agent.sh                  节点 agent 脚本
//
// 目录布局（与 PHP 的 storage_path('app/xray/…') 一致）：
//   <AssetRoot>/manifest.json
//   <AssetRoot>/node-bin/agent/<file>
//   <AssetRoot>/node-bin/<version>/<file>
//   <AssetRoot>/geo/<file>              （GeodataService::dir()）
//   <AssetRoot>/node-install.sh / node-agent.sh

// AssetHandler 提供节点资产端点。
type AssetHandler struct {
	// Root 是 storage/app/xray 的绝对路径。
	Root string
	// GeoRoot 是 geo 数据目录（PANEL_GEODATA_DIR；空则 <Root>/geo）。
	GeoRoot string
}

// NewAssetHandler 构造 AssetHandler；root 为空时用 ../backend/storage/app/xray。
func NewAssetHandler(root, geoRoot string) *AssetHandler {
	if strings.TrimSpace(root) == "" {
		root = filepath.Join("..", "backend", "storage", "app", "xray")
	}
	return &AssetHandler{Root: root, GeoRoot: geoRoot}
}

// assetArchMap 是内核二进制白名单（对齐 ARCH_MAP）。
var assetArchMap = map[string]string{
	"linux-64": "Xray-linux-64.zip",
}

// geoFilePattern 复刻 PHP 路由的 `->where('file', '[a-zA-Z0-9._-]+\.dat')`。
var geoFilePattern = regexp.MustCompile(`^[a-zA-Z0-9._-]+\.dat$`)

// geoFiles 是 geo 分发白名单（对齐 GeodataService::FILES 的键）。
var geoFiles = map[string]bool{
	"geoip.dat":   true,
	"geosite.dat": true,
}

// jsonError 复刻 NodeAssetController 的 `response()->json(['code'=>N,'msg'=>…], N)`。
//
// 注意：这组端点的 http status 就是 code（与 node-api 的「http 200 信封」不同），
// 因为它们是文件下载语义，安装脚本按 http status 判成败。
func jsonError(c *gin.Context, status int, message string) {
	writePHPJSON(c, status, Object{
		{Key: "code", Value: status},
		{Key: "msg", Value: message},
	})
}

// Manifest 复刻 manifest()：原样返回 manifest.json（不套 {code,data} 信封）。
func (h *AssetHandler) Manifest(c *gin.Context) {
	p := filepath.Join(h.Root, "manifest.json")
	f, err := os.Open(p)
	if err != nil {
		jsonError(c, http.StatusNotFound, "asset manifest not found")
		return
	}
	defer f.Close()

	data, err := io.ReadAll(f)
	if err != nil {
		jsonError(c, http.StatusNotFound, "asset manifest not found")
		return
	}
	c.Data(http.StatusOK, "application/json; charset=utf-8", data)
}

// AgentBinary 复刻 agentBinary($arch)：白名单架构 + manifest 查表 + sha256 复验后下发。
func (h *AssetHandler) AgentBinary(c *gin.Context) {
	arch := c.Param("arch")
	if _, ok := assetArchMap[arch]; !ok {
		jsonError(c, http.StatusNotFound, "unknown arch: "+arch)
		return
	}

	manifest, ok := h.readManifest(c)
	if !ok {
		return
	}

	asset := findAsset(manifest, "agent", "", arch)
	if asset == nil {
		jsonError(c, http.StatusNotFound, "agent asset missing: "+arch)
		return
	}

	p := filepath.Join(h.Root, "node-bin", "agent", asset.File)
	h.serveVerified(c, p, asset, "application/octet-stream",
		"agent asset missing on panel", "agent checksum mismatch, refusing to serve")
}

// Binary 复刻 binary($version, $arch)：arch 兼容短名与 `Xray-linux-64` 两种写法。
func (h *AssetHandler) Binary(c *gin.Context) {
	version := c.Param("version")
	// 路由捕获的是 `<arch>.zip` 的 arch 段（gin 的 :arch 不带扩展名，见 Routes）。
	arch := strings.TrimSuffix(c.Param("arch"), ".zip")
	arch = strings.TrimPrefix(arch, "Xray-")

	if _, ok := assetArchMap[arch]; !ok {
		jsonError(c, http.StatusNotFound, "unknown arch: "+arch)
		return
	}

	manifest, ok := h.readManifest(c)
	if !ok {
		return
	}

	asset := findAsset(manifest, "versions", version, arch)
	if asset == nil {
		jsonError(c, http.StatusNotFound, fmt.Sprintf("unknown version/arch: %s/%s", version, arch))
		return
	}

	p := filepath.Join(h.Root, "node-bin", version, asset.File)
	h.serveVerified(c, p, asset, "application/zip",
		"asset missing on panel", "asset checksum mismatch, refusing to serve")
}

// Geo 复刻 geo($file)：白名单文件名 + 存在性检查后下发（带 X-SHA256）。
func (h *AssetHandler) Geo(c *gin.Context) {
	name := c.Param("file")
	if !geoFiles[name] {
		// PHP 侧对不在白名单的文件直接 404（路由 where 已限 .dat，但内容仍需白名单）。
		jsonError(c, http.StatusNotFound, "unknown geo file: "+name)
		return
	}

	p := filepath.Join(h.geoRoot(), name)
	info, err := os.Stat(p)
	if err != nil || info.IsDir() {
		jsonError(c, http.StatusNotFound, "geo file not downloaded yet")
		return
	}

	sum, err := sha256File(p)
	if err != nil {
		jsonError(c, http.StatusNotFound, "geo file not downloaded yet")
		return
	}
	// 头必须在写响应体之前设置（http.ServeFile 之后写的头不会生效）。
	c.Header("X-SHA256", sum)
	c.Header("Content-Type", "application/octet-stream")
	c.FileAttachment(p, name)
}

// NodeInstall 复刻 nodeInstall()：storage/app/xray/node-install.sh。
func (h *AssetHandler) NodeInstall(c *gin.Context) { h.serveScript(c, "node-install.sh") }

// NodeAgent 复刻 nodeAgent()：storage/app/xray/node-agent.sh。
func (h *AssetHandler) NodeAgent(c *gin.Context) { h.serveScript(c, "node-agent.sh") }

func (h *AssetHandler) serveScript(c *gin.Context, name string) {
	p := filepath.Join(h.Root, name)
	if info, err := os.Stat(p); err != nil || info.IsDir() {
		jsonError(c, http.StatusInternalServerError, name+" missing on panel")
		return
	}
	// 与 PHP 的 response()->file($path, ['Content-Type' => 'text/plain; charset=utf-8']) 一致。
	// 注意头必须在 ServeFile 之前设置。
	c.Header("Content-Type", "text/plain; charset=utf-8")
	c.File(p)
}

func (h *AssetHandler) geoRoot() string {
	if strings.TrimSpace(h.GeoRoot) != "" {
		return h.GeoRoot
	}
	return filepath.Join(h.Root, "geo")
}

// manifestAsset 是 manifest 里的一条资产。
type manifestAsset struct {
	Arch   string `json:"arch"`
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type manifestSection struct {
	Assets []manifestAsset `json:"assets"`
}

func (h *AssetHandler) readManifest(c *gin.Context) (map[string]json.RawMessage, bool) {
	p := filepath.Join(h.Root, "manifest.json")
	raw, err := os.ReadFile(p)
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "asset manifest missing on panel")
		return nil, false
	}
	var manifest map[string]json.RawMessage
	if err := json.Unmarshal(raw, &manifest); err != nil {
		// PHP 的 json_decode(…, true) ?? [] 会把损坏的清单当空数组 →
		// 后续 firstWhere 找不到 → 404 "asset missing"。
		return map[string]json.RawMessage{}, true
	}
	return manifest, true
}

// findAsset 复刻 collect($manifest[$section][$key]['assets'] ?? [])->firstWhere('arch', $arch)。
// section="agent" 时 key 忽略（agent 段是平铺的 {"assets":[…]}）。
func findAsset(manifest map[string]json.RawMessage, section, key, arch string) *manifestAsset {
	raw, ok := manifest[section]
	if !ok {
		return nil
	}

	var sectionRaw json.RawMessage = raw
	if key != "" {
		var versions map[string]json.RawMessage
		if err := json.Unmarshal(raw, &versions); err != nil {
			return nil
		}
		entry, ok := versions[key]
		if !ok {
			return nil
		}
		sectionRaw = entry
	}

	var sec manifestSection
	if err := json.Unmarshal(sectionRaw, &sec); err != nil {
		return nil
	}
	for i := range sec.Assets {
		if sec.Assets[i].Arch == arch {
			return &sec.Assets[i]
		}
	}
	return nil
}

// serveVerified 复刻「文件存在 + sha256 复验」后再下发。
func (h *AssetHandler) serveVerified(c *gin.Context, filePath string, asset *manifestAsset, contentType, missingMsg, checksumMsg string) {
	if info, err := os.Stat(filePath); err != nil || info.IsDir() {
		jsonError(c, http.StatusInternalServerError, missingMsg)
		return
	}

	sum, err := sha256File(filePath)
	if err != nil {
		jsonError(c, http.StatusInternalServerError, missingMsg)
		return
	}
	if !strings.EqualFold(sum, asset.SHA256) {
		// 面板侧最后一道校验：仓库被误改/污染时拒发（别把坏文件推给节点）。
		jsonError(c, http.StatusInternalServerError, checksumMsg)
		return
	}

	// 头必须在 ServeFile 之前设置。PHP 用 response()->download() 下发，带
	// Content-Disposition: attachment + 明确 Content-Type。
	c.Header("X-SHA256", asset.SHA256)
	c.Header("Content-Type", contentType)
	c.FileAttachment(filePath, path.Base(asset.File))
}

func sha256File(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// RegisterAssetRoutes 把资产端点挂到 r 上（公开，无鉴权）。
//
// 路由顺序对齐 PHP：agent 段必须排在 {version}/{arch}.zip 之前，
// 但 gin 的静态段优先于参数段，两者不会互相吞（已用真实注册验证）。
func (h *AssetHandler) RegisterRoutes(r gin.IRouter) {
	r.GET("/node-install.sh", h.NodeInstall)
	r.GET("/node-agent.sh", h.NodeAgent)
	r.GET("/node-bin/manifest.json", h.Manifest)
	r.GET("/node-bin/agent/:arch", h.AgentBinary)
	// PHP 侧对 geo 路由有 `->where('file', '[a-zA-Z0-9._-]+\.dat')` 约束：
	// 非 .dat 直接不命中路由（框架级 404），不会走到「未知 geo 文件」分支。
	r.GET("/node-bin/geo/:file", func(c *gin.Context) {
		if !geoFilePattern.MatchString(c.Param("file")) {
			c.Status(http.StatusNotFound)
			return
		}
		h.Geo(c)
	})
	// PHP 的路由是 `/node-bin/{version}/{arch}.zip`：后缀是路由的一部分。
	// gin 不允许「参数段内嵌静态后缀」，因此这里把 `.zip` 拆成独立段再校验，
	// 效果等价（缺后缀/错后缀都不命中 → 404，不会进到 handler）。
	r.GET("/node-bin/:version/:arch", func(c *gin.Context) {
		arch := c.Param("arch")
		if !strings.HasSuffix(arch, ".zip") {
			c.Status(http.StatusNotFound)
			return
		}
		// 把去掉后缀的 arch 写回参数，handler 内的 TrimSuffix 逻辑因此可以简化。
		c.Params = append(c.Params[:0], gin.Param{Key: "version", Value: c.Param("version")},
			gin.Param{Key: "arch", Value: arch})
		h.Binary(c)
	})
}

// RegisterNodeAPIRoutes 把六端点挂到 r 上（node.auth 鉴权）。
func (h *Handler) RegisterRoutes(r gin.IRouter) {
	group := r.Group("/api/node-api", h.NodeAuth())
	group.POST("/register", h.Register)
	group.GET("/config", h.Config)
	group.POST("/push", h.Push)
	group.POST("/alive", h.Alive)
	group.POST("/execute", h.Execute)
	group.POST("/ack", h.Ack)
}
