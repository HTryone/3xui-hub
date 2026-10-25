package admin

// 管理端多域名域（P8c）的类型与依赖。
//
// 逐条对齐 backend/app/Http/Controllers/Admin/DomainController.php 与
// backend/app/Services/DomainService.php（apply/renew/remove 走异步任务）。

import (
	"context"
	"log"
	"time"
)

// DomainRow 是 domains 表的一行。
//
// ssl_status: none | pending | ok | expiring | failed。
type DomainRow struct {
	ID            uint64
	Domain        string
	IsPrimary     bool
	Enabled       bool
	SSLStatus     string
	CertExpiresAt *time.Time
	LastError     *string
	CreatedAt     *time.Time
}

// DomainStore 是域名域对数据库的全部依赖。
type DomainStore interface {
	// DomainsIndex 对齐 Domain::where('enabled',1)->orderByDesc('is_primary')->orderBy('domain')
	DomainsIndex(ctx context.Context) ([]DomainRow, error)
	DomainByID(ctx context.Context, id uint64) (*DomainRow, error)
	// DomainExists 域名唯一性检查（任意 enabled 状态，对齐 Domain::where('domain')->exists()）
	DomainExists(ctx context.Context, domain string) (bool, error)
	// DomainEnabledCount 对齐 Domain::where('enabled',1)->count()
	DomainEnabledCount(ctx context.Context) (int64, error)
	CreateDomain(ctx context.Context, domain string, isPrimary bool) (*DomainRow, error)
	// SetPrimary 对齐事务内「清其他人主域 + 置自己」
	SetPrimary(ctx context.Context, id uint64) error
	// DeleteDomain 删除后返回剩余 enabled 里字典序第一行（删主域时提主用）
	DeleteDomain(ctx context.Context, id uint64) (nextPrimary *DomainRow, err error)
	// MarkSSLUpdating 对齐 apply/renew 前置的 ssl_status=pending
	MarkSSLUpdating(ctx context.Context, id uint64) error
}

// DomainTask 是一次域名异步操作的提交结果（async_tasks 行）。
type DomainTask struct {
	ID uint64
}

// DomainTaskSubmitter 对齐 DomainService::submit（建 async_tasks + 派发）。
//
// 真实实现写 async_tasks 表并通知 worker；测试注入假实现。
type DomainTaskSubmitter interface {
	Submit(ctx context.Context, action string, domainID uint64, domainName string) (*DomainTask, error)
}

// DomainDeps 是域名域的依赖清单。
type DomainDeps struct {
	Store DomainStore
	Tasks DomainTaskSubmitter
	Now   func() time.Time
	Logger loggerLike
}

// DomainsDomain 是多域名域的 HTTP 入口。
type DomainsDomain struct {
	store  DomainStore
	tasks  DomainTaskSubmitter
	now    func() time.Time
	logger loggerLike
}

// NewDomainsDomain 组装多域名域。
func NewDomainsDomain(d DomainDeps) *DomainsDomain {
	if d.Store == nil {
		panic("admin: DomainStore 不能为空")
	}
	tasks := d.Tasks
	if tasks == nil {
		tasks = noopDomainTasks{}
	}
	now := d.Now
	if now == nil {
		now = time.Now
	}
	logger := d.Logger
	if logger == nil {
		logger = log.Default()
	}
	return &DomainsDomain{store: d.Store, tasks: tasks, now: now, logger: logger}
}

// noopDomainTasks 未注入 worker 时的占位（只记日志）。
type noopDomainTasks struct{}

func (noopDomainTasks) Submit(_ context.Context, action string, domainID uint64, domainName string) (*DomainTask, error) {
	_ = action
	_ = domainName
	return &DomainTask{ID: domainID}, nil
}
