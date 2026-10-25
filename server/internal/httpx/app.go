package httpx

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"sync"

	"controlhub-server/internal/config"
	"controlhub-server/internal/crypto"
)

// App 持有跨 handler 共享的依赖（DB 连接池、配置、session）。
//
// 数据库是 database/sql 连接池，并发安全；handler 拿 *sql.DB 直接执行手写 SQL。
// 认证中间件（ApiAuth/AdminAuth/NodeAuth）在后续模块接入，本阶段不注册。
type App struct {
	DB      *sql.DB
	Cfg     *config.Config
	session *SessionStore
}

// NewApp 组装共享依赖。db 可为 nil（仅做无库路由的单测/健康检查）。
func NewApp(db *sql.DB, cfg *config.Config) *App {
	return &App{DB: db, Cfg: cfg, session: NewSessionStore()}
}

// Session 返回 session store（供登录 handler 写入、中间件读取）。
func (a *App) Session() *SessionStore { return a.session }

// DecryptNodeSecret 解密 nodes.driver_config JSON 里的 node_secret 字段
// （driver_config 整体是 JSON 文本，node_secret 是 Crypt::encryptString 的密文）。
// 返回空串表示该节点没有密钥或密文无法解密。
func (a *App) DecryptNodeSecret(driverConfig sql.NullString) string {
	if !driverConfig.Valid || driverConfig.String == "" {
		return ""
	}
	var cfg struct {
		NodeSecret string `json:"node_secret"`
	}
	if err := json.Unmarshal([]byte(driverConfig.String), &cfg); err != nil || cfg.NodeSecret == "" {
		return ""
	}
	plain, err := crypto.DecryptString(cfg.NodeSecret, a.Cfg.AppKey)
	if err != nil {
		return ""
	}
	return plain
}

// SessionStore 内存 session（sessionID → adminID）。
// 并存期不读 Laravel 的 sessions 表（PHP 序列化格式不同），切 Go 后 admin 重新登录一次即可。
type SessionStore struct {
	mu sync.Mutex
	m  map[string]uint64
}

// NewSessionStore 创建空的 session 表。
func NewSessionStore() *SessionStore {
	return &SessionStore{m: make(map[string]uint64)}
}

// Set 写入 session。
func (s *SessionStore) Set(id string, adminID uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[id] = adminID
}

// Get 读取 session。
func (s *SessionStore) Get(id string) (uint64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[id]
	return v, ok
}

// Del 删除 session。
func (s *SessionStore) Del(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, id)
}

// NewSessionID 生成 32 字节随机 session id（hex）。
func NewSessionID() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand 失败意味着系统熵源异常，此时拒绝发 session 比发可预测 id 安全。
		panic("httpx: 生成 session id 失败: " + err.Error())
	}
	return hex.EncodeToString(b)
}
