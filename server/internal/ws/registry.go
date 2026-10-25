package ws

import (
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Registry 是 xray 节点 WS 连接注册表（NodeRegistry.php 的 Go 版）。
//
// PHP 是单进程内存表（Worker count=1）：nodeId → TcpConnection。
// Go 侧同一进程内用互斥锁保护；将来若需多进程，这里换成静态内存 + Redis 路由即可，接口不变。
type Registry struct {
	mu    sync.Mutex
	conns map[int64]*Conn
}

// NewRegistry 构造空注册表。
func NewRegistry() *Registry {
	return &Registry{conns: map[int64]*Conn{}}
}

// Add 注册连接（同一节点重复连接：踢掉旧连接，新连接优先 —— agent 重启场景）。
// 返回被踢掉的旧连接（已从表里移除，调用方负责 close；nil = 没有旧连接）。
func (r *Registry) Add(nodeID int64, conn *Conn) *Conn {
	r.mu.Lock()
	defer r.mu.Unlock()
	var old *Conn
	if prev, ok := r.conns[nodeID]; ok && prev != conn {
		old = prev
	}
	r.conns[nodeID] = conn
	return old
}

// Remove 移除连接。只移除「还是自己」的那一份（断开竞态下表里可能已被新连接顶替）。
func (r *Registry) Remove(nodeID int64, conn *Conn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cur, ok := r.conns[nodeID]; ok && cur == conn {
		delete(r.conns, nodeID)
	}
}

// Get 取节点当前连接（nil = 不在线）。
func (r *Registry) Get(nodeID int64) *Conn {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.conns[nodeID]
}

// Count 是当前连接数。
func (r *Registry) Count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.conns)
}

// All 返回注册表快照（nodeId → conn）。遍历/推送用，调用方不得长期持有。
func (r *Registry) All() map[int64]*Conn {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[int64]*Conn, len(r.conns))
	for id, c := range r.conns {
		out[id] = c
	}
	return out
}

// ConnectedNodeIds 是已连接节点 id 列表（PHP array_keys，升序稳定输出便于对账）。
func (r *Registry) ConnectedNodeIds() []int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := make([]int64, 0, len(r.conns))
	for id := range r.conns {
		ids = append(ids, id)
	}
	// map 遍历无序；排序后与 PHP ksort 语义一致（PHP array_keys 保插入序，
	// 这里按 id 升序 —— 轮询推送的日志与查询顺序因此可复现）。
	sortInt64s(ids)
	return ids
}

func sortInt64s(a []int64) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j] < a[j-1]; j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}

// Conn 是一条已建立的 WS 连接 + 挂在连接上的运行期数据。
//
// 对应 PHP $conn->context 的三个字段（放 context，避免 PHP 8.2+ 动态属性弃用告警）：
//
//	nodeId          鉴权通过后的节点 ID（0 = 未鉴权）
//	lastSeen        最近收到任何消息的时间戳（僵死连接判定）
//	notifiedVersion 已通知过的 config 版本（config.changed 去重）
//
// 另有 statsOverflowLoggedAt：report 超限告警的节流水位（OVERFLOW_LOG_INTERVAL）。
type Conn struct {
	ws *websocket.Conn

	writeMu sync.Mutex // 序列化写（poll/heartbeat/错误消息可能并发写同一条连接）
	node    int64

	mu                  sync.Mutex
	lastSeen            time.Time
	notifiedVersion     int64
	statsOverflowLogged time.Time
	closed              bool
}

func newConn(c *websocket.Conn, nodeID int64, now time.Time) *Conn {
	return &Conn{ws: c, node: nodeID, lastSeen: now}
}

// Send 写一条下行消息（并发安全；连接已关则静默丢弃 —— 与 PHP 的 try-catch 吞异常一致）。
func (c *Conn) Send(payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.ws.WriteMessage(websocket.TextMessage, payload)
}

// Close 关闭底层连接（幂等，重复 close 只生效一次）。
func (c *Conn) Close() error {
	if !c.MarkClosed() {
		return nil
	}
	return c.ws.Close()
}

// NodeID 是鉴权后的节点 ID（0 = 未鉴权）。
func (c *Conn) NodeID() int64 { return c.node }

// Touch 刷新 lastSeen（收到任何消息时调用）。
func (c *Conn) Touch(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastSeen = now
}

// LastSeen 是最近一次收到消息的时间。
func (c *Conn) LastSeen() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastSeen
}

// NotifiedVersion 是已通知过的 config 版本。
func (c *Conn) NotifiedVersion() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.notifiedVersion
}

// SetNotifiedVersion 更新已通知版本（只前进，不回退）。
//
// 用于 handleReport / pollAndPush：版本单调前进，重复通知会被自然抑制。
func (c *Conn) SetNotifiedVersion(v int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if v > c.notifiedVersion {
		c.notifiedVersion = v
	}
}

// ForceNotifiedVersion 直接写入已通知版本（允许回退）。
//
// 对齐 handleHello：`$conn->context->notifiedVersion = max(0, (int)($data['config_version'] ?? 0))`
// —— agent 报自己还在 v5 而面板已到 v12 时，必须回写 5，下一次轮询才会补发
// config.changed；若只允许前进，这条通知会被吞掉。
func (c *Conn) ForceNotifiedVersion(v int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if v < 0 {
		v = 0
	}
	c.notifiedVersion = v
}

// ShouldLogStatsOverflow 复刻 PHP 的 lastLoggedAt 节流：间隔 >= OVERFLOW_LOG_INTERVAL 才记一次。
func (c *Conn) ShouldLogStatsOverflow(now time.Time, interval time.Duration) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.statsOverflowLogged.IsZero() && now.Sub(c.statsOverflowLogged) < interval {
		return false
	}
	c.statsOverflowLogged = now
	return true
}

// MarkClosed 幂等关闭标记（重复 close 只生效一次）。
func (c *Conn) MarkClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return false
	}
	c.closed = true
	return true
}
