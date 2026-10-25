package admin

// 管理端备份域（P8c）的类型与依赖。
//
// 逐条对齐 backend/app/Http/Controllers/Admin/BackupController.php 与
// backend/app/Services/BackupEnvService.php。
//
// 外部工具（mysqldump / mysql / zip / artisan）全部走接缝，单测不碰真库、不跑 shell。

import (
	"context"
	"log"
	"time"
)

// backupUploadBase 是上传文件的落盘名（preview() 写，import() 读）。
const backupUploadBase = "backup_upload"

// backupPreviewRecord 是预览记录的文件名（放在上传文件旁边）。
const backupPreviewRecord = "backup_preview.json"

// backupPreviewSQL 是解析用的中间 SQL 文件名。
const backupPreviewSQL = "backup_preview.sql"

// backupPreviewTTLMinutes 预览记录的有效期（分钟）：超过就得重新上传+重新预览。
const backupPreviewTTLMinutes = 30

// backupDiffTables 是预览差异要统计的表（对齐 calculateDiffFromSql）。
var backupDiffTables = []string{"users", "nodes", "plans", "orders", "payment_configs", "admins", "node_inbounds"}

// BackupDBConfig 是 mysqldump / mysql 需要的连接信息。
type BackupDBConfig struct {
	Host     string
	Port     string
	Database string
	Username string
	Password string
}

// BackupTools 是备份域对宿主机 CLI 工具的接缝。
type BackupTools interface {
	// ExportDump 跑 mysqldump 把整库 dump 到 destPath（stderr 丢弃）。
	ExportDump(ctx context.Context, cfg BackupDBConfig, destPath string) error
	// ImportSQL 把 sqlPath 灌进 cfg.Database。
	ImportSQL(ctx context.Context, cfg BackupDBConfig, sqlPath string) error
	// CreateDatabase / DropDatabase 供 merge 模式的临时库使用。
	CreateDatabase(ctx context.Context, cfg BackupDBConfig, name string) error
	DropDatabase(ctx context.Context, cfg BackupDBConfig, name string) error
}

// BackupZip 是备份包打包/解包的接缝（PHP 的 ZipArchive）。
type BackupZip interface {
	// Build 打包 dump.sql + 可选 .env 条目（envContents 为空则不写 .env）。
	Build(dumpPath, zipPath, envContents string) error
	// ExtractSQL 解出 dump.sql 到 destSQL；返回 .env 原文（无则 hasEnv=false）。
	ExtractSQL(zipPath, destSQL string) (envContents string, hasEnv bool, err error)
	// HasEnv 判断 zip 里有没有 .env 条目。
	HasEnv(zipPath string) bool
	// ReadEnv 读 zip 里 .env 条目原文（无则 hasEnv=false）。
	ReadEnv(zipPath string) (contents string, hasEnv bool, err error)
}

// BackupPostImport 是导入数据后的收尾（migrate / queue:restart）。
type BackupPostImport interface {
	RunPendingMigrations(ctx context.Context) (ran int, err error)
	RestartQueueWorkers(ctx context.Context) error
}

// BackupFileStore 是备份域的磁盘操作接缝（tmp 目录 + .env 文件）。
//
// 抽成接缝是因为 .env 的路径与 tmp 目录在真实部署里是固定的（base_path('.env') /
// storage/app/tmp），单测要能在临时目录里跑完整流程。
type BackupFileStore interface {
	// TmpDir 返回（并保证存在）备份临时目录。
	TmpDir() (string, error)
	// WriteFile / ReadFile / Remove / Exists / Size / SHA256 / Copy 覆盖 PHP 的 File::* 用法。
	WriteFile(path string, data []byte) error
	ReadFile(path string) ([]byte, error)
	Remove(path string) error
	Exists(path string) bool
	Size(path string) int64
	SHA256(path string) string
	CopyFile(src, dst string) error
}

// BackupStore 是备份域对数据库的依赖（预览差异的当前行数）。
type BackupStore interface {
	// TableCounts 对齐 calculateDiffFromSql 里 DB::table($table)->count()。
	TableCounts(ctx context.Context, tables []string) (map[string]int64, error)
}

// BackupDeps 是备份域的依赖清单。
type BackupDeps struct {
	Store   BackupStore
	Tools   BackupTools
	Zip     BackupZip
	Post    BackupPostImport
	Files   BackupFileStore
	// DB 是导出/导入用的 MySQL 连接配置（来自 config/database.php）。
	DB BackupDBConfig
	// EnvPath 是本机 .env 路径（BackupEnvService 的 $envPath）。
	EnvPath string
	// AppKey 仅供 BackupEnvService 不需要；.env 里的 APP_KEY 直接读文件。
	Now    func() time.Time
	Logger loggerLike
}

// BackupDomain 是备份域的 HTTP 入口。
type BackupDomain struct {
	store BackupStore
	tools BackupTools
	zip   BackupZip
	post  BackupPostImport
	files BackupFileStore
	db    BackupDBConfig
	env   *BackupEnvService
	now   func() time.Time
	logger loggerLike

	// envNotice 是本次导入 .env 恢复的提示（给前端显示）。
	envNotice *string
}

// NewBackupDomain 组装备份域。
func NewBackupDomain(d BackupDeps) *BackupDomain {
	if d.Store == nil || d.Tools == nil || d.Zip == nil || d.Files == nil {
		panic("admin: BackupDomain 依赖不全")
	}
	post := d.Post
	if post == nil {
		post = noopBackupPost{}
	}
	now := d.Now
	if now == nil {
		now = time.Now
	}
	logger := d.Logger
	if logger == nil {
		logger = log.Default()
	}
	envPath := d.EnvPath
	if envPath == "" {
		envPath = ".env"
	}
	return &BackupDomain{
		store:  d.Store,
		tools:  d.Tools,
		zip:    d.Zip,
		post:   post,
		files:  d.Files,
		db:     d.DB,
		env:    NewBackupEnvService(envPath, d.Files, logger),
		now:    now,
		logger: logger,
	}
}

// noopBackupPost 未注入收尾动作时的占位（报告 0 个迁移、队列未重启）。
type noopBackupPost struct{}

func (noopBackupPost) RunPendingMigrations(context.Context) (int, error) { return 0, nil }
func (noopBackupPost) RestartQueueWorkers(context.Context) error        { return nil }

// ---- 预览记录（backup_preview.json）----

// backupPreviewData 是预览记录的 JSON 结构。
type backupPreviewData struct {
	File        string `json:"file"`
	SHA256      string `json:"sha256"`
	Size        int64  `json:"size"`
	PreviewedAt int64  `json:"previewed_at"`
	ExpiresAt   int64  `json:"expires_at"`
}
