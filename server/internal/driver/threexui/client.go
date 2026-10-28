// Package threexui 是 3x-ui v3.x 面板驱动的 Go 实现。
//
// 逐条对齐 backend/app/Services/ThreeXUi/ThreeXUiClient.php（1216 行）与
// backend/app/Drivers/ThreeXUi/ThreeXUiDriver.php：
//
//   - 鉴权：优先 Bearer API Token（nodes.api_key），无状态、跳过 CSRF；
//     仅当未提供 api_key 时回退 cookie + CSRF 登录（/login + /panel/api/csrf-token）。
//   - 统一响应 {success,msg,obj}：success===true 取 obj，否则按文案判定 not-found。
//   - client 主键 = email（ControlHub 用 ch_user_{user.id}）；uuid/password 由面板生成。
//   - 批量流量同步只发一次 listInbounds，且**同 email 跨入站只取首次**（不累加）。
//
// Go 侧的差异（有意，逐条写清理由）：
//   - cookie 登录态缓存：PHP 用 Laravel cache 做跨进程共享。Go 侧同一进程内用内存
//     map + TTL 实现同一语义（键与 PHP 的 authCacheKey 一致），不引入外部缓存依赖；
//     接口 AuthCache 可注入，将来接 Redis 时无需改本文件。
//   - 没有 async 方法：Go 用 ctx + goroutine 并发，Bearer 模式天然无状态。
//   - 超时从 Config 注入，不读 Laravel config()（Go 侧没有 config 门面）。
package threexui

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"controlhub-server/internal/driver/internal/phpjson"
)

// 端点路径常量（集中管理，/panel/api/* 前缀）。与 PHP 侧逐字一致。
const (
	EPClientsList    = "/panel/api/clients/list"
	EPClientsGet     = "/panel/api/clients/get/" // + {email}
	EPClientsAdd     = "/panel/api/clients/add"
	EPClientsUpdate  = "/panel/api/clients/update/"       // + {email}
	EPClientsDel     = "/panel/api/clients/del/"          // + {email}
	EPClientsAttach  = "/panel/api/clients/"              // + {email}/attach
	EPClientsDetach  = "/panel/api/clients/"              // + {email}/detach
	EPClientsReset   = "/panel/api/clients/resetTraffic/" // + {email}
	EPClientsLinks   = "/panel/api/clients/links/"        // + {email}
	EPClientsTraffic = "/panel/api/clients/traffic/"      // + {email}
	EPClientsOnlines = "/panel/api/clients/onlines"

	EPInboundsList    = "/panel/api/inbounds/list"
	EPInboundsOptions = "/panel/api/inbounds/options"
	EPInboundsGet     = "/panel/api/inbounds/get/" // + {id}

	EPServerStatus  = "/panel/api/server/status"
	EPServerNewUUID = "/panel/api/server/getNewUUID"

	// cookie 登录（兜底）
	epLogin     = "/login"
	epCSRFToken = "/panel/api/csrf-token"
)

// 默认超时（秒），与 backend/config/panel.php 的默认值一致。
const (
	defaultAPITimeout    = 15.0
	defaultHealthTimeout = 8.0
	defaultLoginTimeout  = 5.0
	defaultCSRFTimeout   = 2.0
	defaultAuthCacheTTL  = 300
)

// Config 是构造 Client 的配置（对齐 ThreeXUiClient::__construct 的 $config 数组）。
type Config struct {
	Scheme      string
	Host        string
	Port        int
	WebBasePath string
	// APIKey 为空时回退 cookie 登录。
	APIKey   string
	Username string
	Password string
	// VerifySSL 默认 false（对齐 ThreeXUiClient::fromNode() 的 `$node->verify_ssl ?? false`）。
	VerifySSL bool

	// 超时（秒）；0 表示用默认值。
	APITimeout    float64
	HealthTimeout float64
	LoginTimeout  float64
	CSRFTimeout   float64
	AuthCacheTTL  int

	// HTTPClient 可注入（测试用）；nil 时按配置自建（Transport 尊重 VerifySSL）。
	HTTPClient *http.Client
	// AuthCache 可注入（测试用）；nil 时用进程内默认缓存。
	AuthCache AuthCache
	// Now 可注入（测试固定时间用）。
	Now func() time.Time
}

// AuthCache 是 cookie 登录态的跨请求缓存（对齐 Laravel cache 的 get/put/forget）。
type AuthCache interface {
	Get(key string) (any, bool)
	Put(key string, value any, ttl time.Duration)
	Forget(key string)
}

// authPayload 是缓存里的登录态载荷（对齐 PHP 的 ['cookies' => [...], 'csrf' => ?string]）。
type authPayload struct {
	Cookies []*http.Cookie
	CSRF    string
}

// Client 是 3x-ui HTTP API 客户端。
type Client struct {
	baseURL  string
	apiKey   string
	username string
	password string
	verify   bool
	http     *http.Client
	cache    AuthCache
	now      func() time.Time

	apiTimeout    time.Duration
	healthTimeout time.Duration
	loginTimeout  time.Duration
	csrfTimeout   time.Duration
	cacheTTL      time.Duration

	// 面板指纹成分（登录态缓存键用；客户端拿不到 node id，故不依赖它）
	scheme   string
	host     string
	port     int
	basePath string

	// cookie 模式登录态
	mu            sync.Mutex
	authenticated bool
	cookies       []*http.Cookie
	csrfToken     string
}

// memoryAuthCache 是进程内登录态缓存（PHP 侧是 Laravel cache；语义等价：TTL、可清）。
type memoryAuthCache struct {
	mu    sync.Mutex
	items map[string]memoryAuthItem
	now   func() time.Time
}

type memoryAuthItem struct {
	value   any
	expires time.Time
}

func newMemoryAuthCache(now func() time.Time) *memoryAuthCache {
	return &memoryAuthCache{items: map[string]memoryAuthItem{}, now: now}
}

func (c *memoryAuthCache) Get(key string) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	it, ok := c.items[key]
	if !ok {
		return nil, false
	}
	if !it.expires.IsZero() && c.now().After(it.expires) {
		delete(c.items, key)
		return nil, false
	}
	return it.value, true
}

func (c *memoryAuthCache) Put(key string, value any, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	it := memoryAuthItem{value: value}
	if ttl > 0 {
		it.expires = c.now().Add(ttl)
	}
	c.items[key] = it
}

func (c *memoryAuthCache) Forget(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.items, key)
}

// New 构造客户端。baseURL 规则与 PHP 完全一致：
//
//	"{scheme}://{host}:{port}{web_base_path}"，web_base_path 去尾斜杠、无前导斜杠则补上。
func New(cfg Config) *Client {
	scheme := cfg.Scheme
	if scheme == "" {
		scheme = "https"
	}
	port := cfg.Port
	if port == 0 {
		port = 443
	}
	basePath := strings.TrimRight(cfg.WebBasePath, "/")
	if basePath != "" && !strings.HasPrefix(basePath, "/") {
		basePath = "/" + basePath
	}

	now := cfg.Now
	if now == nil {
		now = time.Now
	}

	// baseURL 的 host:port 走 net.JoinHostPort：域名/IPv4 输出与 "%s:%d" 逐字节一致，
	// IPv6 字面量自动补方括号（否则 "https://2602:...:1fc5:48972" 会被切成主机名+端口）。
	c := &Client{
		scheme:        scheme,
		host:          cfg.Host,
		port:          port,
		basePath:      basePath,
		baseURL:       fmt.Sprintf("%s://%s%s", scheme, net.JoinHostPort(cfg.Host, strconv.Itoa(port)), basePath),
		apiKey:        cfg.APIKey,
		username:      cfg.Username,
		password:      cfg.Password,
		verify:        cfg.VerifySSL,
		now:           now,
		cache:         cfg.AuthCache,
		apiTimeout:    seconds(cfg.APITimeout, defaultAPITimeout),
		healthTimeout: seconds(cfg.HealthTimeout, defaultHealthTimeout),
		loginTimeout:  seconds(cfg.LoginTimeout, defaultLoginTimeout),
		csrfTimeout:   seconds(cfg.CSRFTimeout, defaultCSRFTimeout),
		cacheTTL:      time.Duration(ttlOrDefault(cfg.AuthCacheTTL)) * time.Second,
	}
	if c.cache == nil {
		c.cache = newMemoryAuthCache(now)
	}
	c.http = cfg.HTTPClient
	if c.http == nil {
		c.http = &http.Client{
			Timeout:   c.apiTimeout,
			Transport: defaultTransport(cfg.VerifySSL),
		}
	}
	return c
}

func seconds(v, def float64) time.Duration {
	if v <= 0 {
		v = def
	}
	return time.Duration(v * float64(time.Second))
}

func ttlOrDefault(ttl int) int {
	if ttl == 0 {
		return defaultAuthCacheTTL
	}
	return ttl
}

// defaultTransport 按 verify_ssl 决定是否跳过证书校验
// （dev 联调可传 false 绕过自签/不完整证书链；生产默认即 false 与 PHP 一致）。
func defaultTransport(verify bool) http.RoundTripper {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if !verify {
		tr.TLSClientConfig = insecureTLSConfig()
	}
	return tr
}

// BaseURL 返回拼接后的面板根地址（测试与诊断用）。
func (c *Client) BaseURL() string { return c.baseURL }

// ── HTTP 底层 ────────────────────────────────────────────────

// send 执行一次 HTTP 请求并返回原始 body（对应 ThreeXUiClient::send/execute）。
//
// cookie 模式下业务请求回 401/403 时：清缓存 → 重新登录一次 → **只重试该请求一次**。
// Bearer 模式不参与（token 无效就该报错）。重试后仍 401 则按普通失败抛出，
// 避免被 requestNullable 的降级分支当成 not-found 吞掉。
func (c *Client) send(ctx context.Context, method, path string, body []byte, timeout time.Duration) ([]byte, error) {
	if err := c.ensureAuthenticated(ctx); err != nil {
		return nil, err
	}

	raw, err := c.execute(ctx, method, path, body, timeout, false)
	if err != nil {
		var expired *AuthExpiredError
		if errors.As(err, &expired) {
			c.forgetAuth()
			if err := c.ensureAuthenticated(ctx); err != nil {
				return nil, err
			}
			raw, err = c.execute(ctx, method, path, body, timeout, true)
		}
	}
	return raw, err
}

// execute 是单次往返（不含重登重试）。isRetry=true 时 401/403 不再触发二次重登。
func (c *Client) execute(ctx context.Context, method, path string, body []byte, timeout time.Duration, isRetry bool) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, requestFailed(err)
	}
	req.Header.Set("Accept", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}

	c.mu.Lock()
	csrf := c.csrfToken
	cookieSnapshot := c.cookies
	c.mu.Unlock()

	// cookie 模式：POST 带 X-CSRF-Token，GET 不需要
	if csrf != "" && strings.EqualFold(method, http.MethodPost) {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	if c.apiKey == "" {
		for _, ck := range cookieSnapshot {
			req.AddCookie(ck)
		}
	}

	client := c.http
	if timeout > 0 {
		// 单请求超时覆盖（PHP 侧每个方法传不同 timeout）。
		// Go 的 http.Client.Timeout 是整次往返上限，这里克隆一个带超时的 client。
		cp := *c.http
		cp.Timeout = timeout
		client = &cp
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, &TransportError{Err: err}
	}
	defer resp.Body.Close()

	raw, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return nil, &TransportError{Err: readErr}
	}

	if c.apiKey == "" && !isRetry &&
		(resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
		return nil, &AuthExpiredError{
			Message: fmt.Sprintf("3x-ui session expired: HTTP %d", resp.StatusCode),
		}
	}
	// http_errors=true 的等价物：HTTP 非 2xx 在 PHP 里是 Guzzle 的 RequestException，
	// execute() 把它包成 ThreeXUiException('3x-ui request failed: ...')。
	//
	// **必须是 *Error 而不是 *TransportError**：PHP 的 requestNullable 只 catch
	// ThreeXUiException，所以 4xx/5xx 会被降级成 null；只有连接层失败
	// （ConnectException 继承 TransferException，不是 RequestException）才穿透。
	// 这条边界决定了「节点返回 502」是「该用户不存在」还是「本次查询失败」。
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, requestFailed(fmt.Errorf("HTTP %d: %s", resp.StatusCode, firstBytes(raw, 200)))
	}
	return raw, nil
}

// envelope 是 3x-ui 的统一响应体 {success,msg,obj}。
type envelope struct {
	Success bool
	Msg     string
	Obj     any
}

// parseEnvelope 解析信封。ok=false 表示非 JSON / 缺少 success 键。
func parseEnvelope(raw []byte) (envelope, bool) {
	v, err := phpjson.Decode(raw)
	if err != nil {
		return envelope{}, false
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return envelope{}, false
	}
	rawSuccess, has := obj["success"]
	if !has {
		return envelope{}, false
	}
	msg := ""
	if m, ok := obj["msg"].(string); ok {
		msg = m
	}
	return envelope{Success: truthy(rawSuccess), Msg: msg, Obj: obj["obj"]}, true
}

// truthy 复刻 PHP 对 success 字段的布尔判定（面板可能给 true/false、1/0、"1"/"0"）。
func truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != "" && t != "0"
	case interface{ String() string }:
		return t.String() != "" && t.String() != "0"
	default:
		return true
	}
}

// request 复刻 ThreeXUiClient::request()：success===true 返回 obj，否则抛 Error(msg)。
func (c *Client) request(ctx context.Context, method, path string, body []byte, timeout time.Duration) (any, error) {
	raw, err := c.send(ctx, method, path, body, timeout)
	if err != nil {
		return nil, err
	}
	env, ok := parseEnvelope(raw)
	if !ok {
		return nil, invalidResponse(raw)
	}
	if !env.Success {
		msg := env.Msg
		if msg == "" {
			msg = "3x-ui request failed"
		}
		return nil, &Error{Message: msg}
	}
	return env.Obj, nil
}

// requestNullable 复刻 requestNullable()：client 不存在 / 任何业务错误 → (nil, nil)。
//
// 与 PHP 的边界一致：**只吞 Error 家族**，传输层失败要原样上抛
// （PHP 的 ConnectException 继承 TransferException，catch (ThreeXUiException) 接不住它）。
func (c *Client) requestNullable(ctx context.Context, method, path string, body []byte, timeout time.Duration) (any, error) {
	raw, err := c.send(ctx, method, path, body, timeout)
	if err != nil {
		var terr *TransportError
		if errors.As(err, &terr) {
			return nil, err
		}
		return nil, nil
	}
	env, ok := parseEnvelope(raw)
	if !ok || !env.Success {
		return nil, nil
	}
	return env.Obj, nil
}

// ── 登录态 ───────────────────────────────────────────────────

// ensureAuthenticated 复刻 ensureAuthenticated()：Bearer 直接通过且**不碰缓存**；
// cookie 模式优先复用缓存，没有才真登录。
func (c *Client) ensureAuthenticated(ctx context.Context) error {
	if c.apiKey != "" {
		return nil
	}
	c.mu.Lock()
	if c.authenticated {
		c.mu.Unlock()
		return nil
	}
	c.mu.Unlock()

	if c.restoreAuthFromCache() {
		c.mu.Lock()
		c.authenticated = true
		c.mu.Unlock()
		return nil
	}

	if err := c.login(ctx); err != nil {
		return err
	}
	c.mu.Lock()
	c.authenticated = true
	c.mu.Unlock()
	c.storeAuthToCache()
	return nil
}

// login 复刻 login()：POST /login（JSON）→ 成功后 GET /panel/api/csrf-token。
func (c *Client) login(ctx context.Context) error {
	body, err := phpjson.EncodeRequest(map[string]any{
		"username": c.username,
		"password": c.password,
	})
	if err != nil {
		return loginFailed(err.Error())
	}

	resp, status, loginErr := c.loginRequest(ctx, http.MethodPost, epLogin, body, c.loginTimeout)
	if loginErr != nil {
		return loginTransportFailed(loginErr)
	}
	if status >= 400 && status != http.StatusUnprocessableEntity {
		// 3x-ui 登录失败常回 200 + success=false；回 4xx/5xx 说明面板/反代层面就拒绝了。
		return loginFailed(fmt.Sprintf("HTTP %d", status))
	}

	env, ok := parseEnvelope(resp)
	if !ok || !env.Success {
		msg := ""
		if ok {
			msg = env.Msg
		}
		return loginFailed(msg)
	}

	// 取 CSRF token（响应体可能是带引号字符串）；失败不致命（部分版本无此端点）
	csrfBody, _, csrfErr := c.loginRequest(ctx, http.MethodGet, epCSRFToken, nil, c.csrfTimeout)
	if csrfErr == nil {
		trimmed := strings.Trim(string(csrfBody), "\" \r\n")
		c.mu.Lock()
		c.csrfToken = trimmed
		c.mu.Unlock()
	}
	return nil
}

// loginRequest 发一次登录阶段请求（独立短超时、携带已收集的 cookie、吸收 Set-Cookie）。
func (c *Client) loginRequest(ctx context.Context, method, path string, body []byte, timeout time.Duration) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, ck := range c.snapshotCookies() {
		req.AddCookie(ck)
	}

	cp := *c.http
	if timeout > 0 {
		cp.Timeout = timeout
	}

	resp, err := cp.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	c.absorbCookies(resp)

	raw, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return nil, resp.StatusCode, readErr
	}
	return raw, resp.StatusCode, nil
}

// absorbCookies 把响应的 Set-Cookie 合并进本实例（同 name+path 覆盖，对齐 jQuery-cookie jar）。
func (c *Client) absorbCookies(resp *http.Response) {
	added := resp.Cookies()
	if len(added) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, ck := range added {
		replaced := false
		for i, old := range c.cookies {
			if old.Name == ck.Name && old.Path == ck.Path {
				c.cookies[i] = ck
				replaced = true
				break
			}
		}
		if !replaced {
			c.cookies = append(c.cookies, ck)
		}
	}
}

func (c *Client) snapshotCookies() []*http.Cookie {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*http.Cookie, len(c.cookies))
	copy(out, c.cookies)
	return out
}

// authCacheKey 复刻 authCacheKey()：sha1(scheme\0host\0port\0basePath\0username)。
// 刻意不掺 node id（客户端拿不到）；不含 password（改密码后旧会话失效由 401 自愈）。
func (c *Client) authCacheKey() string {
	h := sha1.New()
	h.Write([]byte(strings.Join([]string{
		c.scheme, c.host, strconv.Itoa(c.port), c.basePath, c.username,
	}, "\x00")))
	return "panel-auth:" + hex.EncodeToString(h.Sum(nil))
}

func (c *Client) restoreAuthFromCache() bool {
	v, ok := c.cache.Get(c.authCacheKey())
	if !ok {
		return false
	}
	payload, ok := v.(*authPayload)
	if !ok || payload == nil || len(payload.Cookies) == 0 {
		return false
	}
	c.mu.Lock()
	c.cookies = payload.Cookies
	c.csrfToken = payload.CSRF
	c.mu.Unlock()
	return true
}

func (c *Client) storeAuthToCache() {
	if c.cacheTTL <= 0 {
		return
	}
	ck := c.snapshotCookies()
	if len(ck) == 0 {
		return // 没拿到任何 cookie：缓存了也没用（下次照样过不了鉴权）
	}
	c.mu.Lock()
	csrf := c.csrfToken
	c.mu.Unlock()
	c.cache.Put(c.authCacheKey(), &authPayload{Cookies: ck, CSRF: csrf}, c.cacheTTL)
}

func (c *Client) forgetAuth() {
	c.mu.Lock()
	c.authenticated = false
	c.csrfToken = ""
	c.cookies = nil
	c.mu.Unlock()
	c.cache.Forget(c.authCacheKey())
}

// ── 工具 ─────────────────────────────────────────────────────

// escapeEmail 对应 rawurlencode()：email 里可能出现 '@' 与 '+'，必须转义。
func escapeEmail(email string) string { return url.PathEscape(email) }
