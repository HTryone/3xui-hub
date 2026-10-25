package admin

// 管理端备份（/admin-api/backup…）：export / preview / import。
//
// 逐条对齐 backend/app/Http/Controllers/Admin/BackupController.php。
// mysqldump / mysql / zip / migrate 全部走接缝（BackupTools / BackupZip / BackupPostImport），
// 单测只验收流程编排与 SQL 计数状态机，不碰真库。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"controlhub-server/internal/httpx"

	"github.com/gin-gonic/gin"
)

// Register 把备份域路由挂到给定分组。
func (d *BackupDomain) Register(r gin.IRouter) {
	r.GET("/backup/export", d.backupExport)
	r.POST("/backup/preview", d.backupPreview)
	r.POST("/backup/import", d.backupImport)
}

// ---- 响应封装 ----

func (d *BackupDomain) success(c *gin.Context, data any, msg ...string) {
	m := "ok"
	if len(msg) > 0 && msg[0] != "" {
		m = msg[0]
	}
	writeJSON(c, phpEncode(jo("code", 0, "msg", m, "data", data)))
}

func (d *BackupDomain) fail(c *gin.Context, msg string, code int) {
	writeJSON(c, phpEncode(jo("code", code, "msg", msg, "data", nil)))
}

func (d *BackupDomain) failValidation(c *gin.Context, summary string) { d.fail(c, summary, 500) }

func (d *BackupDomain) failServer(c *gin.Context, what string, err error) {
	d.logger.Printf("admin %s 失败: %v", what, err)
	c.Set(httpx.OpLogErrorKey, what+": "+err.Error())
	d.fail(c, "服务器错误", 500)
}

// ---- 磁盘路径 ----

func (d *BackupDomain) uploadPath(tmpDir, ext string) string {
	return filepath.Join(tmpDir, backupUploadBase+"."+ext)
}

func (d *BackupDomain) previewRecordPath(tmpDir string) string {
	return filepath.Join(tmpDir, backupPreviewRecord)
}

func (d *BackupDomain) previewSQLPath(tmpDir string) string {
	return filepath.Join(tmpDir, backupPreviewSQL)
}

// findUploadedBackup 找到当前暂存的上传备份（preview() 落盘的那份）。
func (d *BackupDomain) findUploadedBackup(tmpDir string) (string, string) {
	for _, ext := range []string{"zip", "sql"} {
		candidate := d.uploadPath(tmpDir, ext)
		if d.files.Exists(candidate) {
			return candidate, ext
		}
	}
	return "", ""
}

// clearPreviewArtifacts 清掉磁盘上一切与上传/预览有关的中间文件。
func (d *BackupDomain) clearPreviewArtifacts(tmpDir string) {
	for _, ext := range []string{"zip", "sql"} {
		_ = d.files.Remove(d.uploadPath(tmpDir, ext))
	}
	_ = d.files.Remove(d.previewSQLPath(tmpDir))
	_ = d.files.Remove(d.previewRecordPath(tmpDir))
}

// writePreviewRecord 记下「这份上传刚刚被预览过」（放磁盘文件，不受 cache/session 影响）。
func (d *BackupDomain) writePreviewRecord(tmpDir, uploadPath string) error {
	now := d.now().UTC()
	rec := backupPreviewData{
		File:        filepath.Base(uploadPath),
		SHA256:      d.files.SHA256(uploadPath),
		Size:        d.files.Size(uploadPath),
		PreviewedAt: now.Unix(),
		ExpiresAt:   now.Add(backupPreviewTTLMinutes * time.Minute).Unix(),
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return d.files.WriteFile(d.previewRecordPath(tmpDir), raw)
}

// consumePreviewRecord 校验「这次 import 用的就是刚预览过的那份上传」，校验完记录即作废（一次性）。
// 返回拒绝原因；空串表示校验通过。
func (d *BackupDomain) consumePreviewRecord(tmpDir, uploadPath string) string {
	retry := "，请重新上传备份文件、预览差异后再导入"
	recordPath := d.previewRecordPath(tmpDir)

	if !d.files.Exists(recordPath) {
		return "没有找到本次上传的预览记录" + retry
	}
	raw, err := d.files.ReadFile(recordPath)
	// 一次性：不论校验过不过，这份记录都不再复用
	_ = d.files.Remove(recordPath)
	if err != nil {
		return "预览记录已损坏" + retry
	}
	var rec backupPreviewData
	if err := json.Unmarshal(raw, &rec); err != nil {
		return "预览记录已损坏" + retry
	}
	if rec.ExpiresAt < d.now().UTC().Unix() {
		return fmt.Sprintf("这份上传的预览已超过 %d 分钟%s", backupPreviewTTLMinutes, retry)
	}
	if rec.File != filepath.Base(uploadPath) || rec.SHA256 != d.files.SHA256(uploadPath) {
		return "上传的备份文件与预览过的不是同一份" + retry
	}
	return ""
}

// ---- export ----

// backupExport 对齐 GET /admin-api/backup/export（zip 附件下载）。
func (d *BackupDomain) backupExport(c *gin.Context) {
	filename := "controlhub-backup-" + d.now().UTC().Format("20060102150405") + ".zip"
	tmpDir, err := d.files.TmpDir()
	if err != nil {
		d.failServer(c, "创建临时目录", err)
		return
	}
	dumpPath := d.previewSQLPath(tmpDir) // 与解析用临时 SQL 同名，用完即删
	if err := d.tools.ExportDump(ctxOf(c), d.db, dumpPath); err != nil {
		_ = d.files.Remove(dumpPath)
		d.fail(c, fmt.Sprintf("数据库导出失败（%v）", err), 500)
		return
	}
	if !d.files.Exists(dumpPath) || d.files.Size(dumpPath) == 0 {
		_ = d.files.Remove(dumpPath)
		d.fail(c, "数据库导出失败（mysqldump 无输出）", 500)
		return
	}

	zipPath := filepath.Join(tmpDir, filename)
	envContents := d.env.AppKeyOnlyContents()
	if err := d.zip.Build(dumpPath, zipPath, envContents); err != nil {
		_ = d.files.Remove(dumpPath)
		d.fail(c, err.Error(), 500)
		return
	}
	_ = d.files.Remove(dumpPath)

	content, err := d.files.ReadFile(zipPath)
	if err != nil {
		_ = d.files.Remove(zipPath)
		d.fail(c, "读取备份文件失败", 500)
		return
	}
	_ = d.files.Remove(zipPath)

	c.Header("Content-Type", "application/zip")
	c.Header("Content-Disposition", `attachment; filename="`+filename+`"`)
	c.Header("Content-Length", fmt.Sprintf("%d", len(content)))
	c.Data(http.StatusOK, "application/zip", content)
}

// ---- preview ----

// backupPreview 对齐 POST /admin-api/backup/preview（multipart 上传）。
func (d *BackupDomain) backupPreview(c *gin.Context) {
	file, header, err := c.Request.FormFile("file")
	if err != nil {
		d.failValidation(c, "The file field is required.")
		return
	}
	defer file.Close()
	if header.Size > 50*1024*1024 {
		d.failValidation(c, "The file must not be greater than 51200 kilobytes.")
		return
	}

	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(header.Filename), "."))
	if ext != "zip" && ext != "sql" {
		// PHP 只按扩展名分支；其它扩展名走 extractBackupFile 的「不支持」错误
		ext = "bin"
	}

	tmpDir, err := d.files.TmpDir()
	if err != nil {
		d.failServer(c, "创建临时目录", err)
		return
	}
	// 先把上一轮留下的东西全清掉：盘上任何时刻只允许存在「本次这一份」
	d.clearPreviewArtifacts(tmpDir)

	savedPath := d.uploadPath(tmpDir, ext)
	rawUpload := make([]byte, 0, header.Size)
	buf := make([]byte, 32*1024)
	for {
		n, rerr := file.Read(buf)
		if n > 0 {
			rawUpload = append(rawUpload, buf[:n]...)
		}
		if rerr != nil {
			break
		}
	}
	if err := d.files.WriteFile(savedPath, rawUpload); err != nil {
		d.fail(c, "无法保存备份文件", 400)
		return
	}

	sqlPath, _, extractErr := d.extractBackupFile(savedPath, ext, false)
	if extractErr != nil {
		d.clearPreviewArtifacts(tmpDir)
		d.fail(c, "无法读取备份文件："+extractErr.Error(), 400)
		return
	}

	diff, diffErr := d.calculateDiffFromSql(ctxOf(c), sqlPath)
	// 解析用的 SQL 是用户整库数据，预览完立刻删
	_ = d.files.Remove(sqlPath)
	if diffErr != nil {
		d.clearPreviewArtifacts(tmpDir)
		d.fail(c, "无法分析备份文件："+diffErr.Error(), 400)
		return
	}

	hasEnv := false
	if ext == "zip" {
		hasEnv = d.zip.HasEnv(savedPath)
	}

	var backupEnv map[string]string
	envDiff := jsArr{}
	if hasEnv {
		contents, ok, rerr := d.zip.ReadEnv(savedPath)
		if rerr == nil && ok {
			backupEnv = ParseEnvContent(contents)
			envDiff = d.env.EnvDiff(backupEnv)
		}
	}
	currentEnv := d.env.ParseEnv(d.env.EnvPath())

	if err := d.writePreviewRecord(tmpDir, savedPath); err != nil {
		d.failServer(c, "写入预览记录", err)
		return
	}

	out := jo(
		"diff", diff,
		"has_env", hasEnv,
		"env_diff", envDiff,
		// 站点地址默认填本机当前值；备份里的旧域名只作提示
		"current_app_url", currentEnv["APP_URL"],
		"backup_app_url", envURL(backupEnv),
	)
	d.success(c, out)
}

func envURL(env map[string]string) string {
	if env == nil {
		return ""
	}
	return env["APP_URL"]
}

// extractBackupFile 解压备份文件，返回 SQL 文件路径与 .env 恢复提示（空串 = 正常）。
// 兼容 .zip 和 .sql。restoreEnv=true 时才从备份恢复 APP_KEY（仅导入时）。
func (d *BackupDomain) extractBackupFile(filePath, ext string, restoreEnv bool) (string, string, error) {
	tmpDir, err := d.files.TmpDir()
	if err != nil {
		return "", "", err
	}
	switch ext {
	case "zip":
		sqlTmp := d.previewSQLPath(tmpDir)
		envContents, hasEnv, zerr := d.zip.ExtractSQL(filePath, sqlTmp)
		if zerr != nil {
			return "", "", zerr
		}
		notice := ""
		if restoreEnv {
			var contents *string
			if hasEnv {
				contents = &envContents
			}
			notice = d.env.RestoreAppKeyFromEnvContents(contents)
		}
		return sqlTmp, notice, nil
	case "sql":
		sqlTmp := d.previewSQLPath(tmpDir)
		if err := d.files.CopyFile(filePath, sqlTmp); err != nil {
			return "", "", err
		}
		return sqlTmp, "", nil
	default:
		return "", "", fmt.Errorf("不支持的备份文件格式：%s", ext)
	}
}

// ---- SQL dump 行数统计（状态机，对齐 feedSqlChar）----

const (
	sqlPhaseIdle = iota
	sqlPhaseInto
	sqlPhaseTable
	sqlPhaseHead
	sqlPhaseValues
)

type sqlCountState struct {
	phase   int
	table   string
	name    string
	quoted  bool
	depth   int
	inStr   bool
	escape  bool
	word    string
	counts  map[string]int64
}

func newSQLCountState() *sqlCountState {
	return &sqlCountState{phase: sqlPhaseIdle, counts: map[string]int64{}}
}

func isSQLWordChar(ch byte) bool {
	return (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') ||
		(ch >= '0' && ch <= '9') || ch == '_' || ch == '$' || ch >= 0x80
}

// feedSQLChar 把一个字符喂给解析状态机（状态跨行保持，跨行 INSERT 也能数对）。
func (s *sqlCountState) feedSQLChar(ch byte) {
	// 字符串内容不是 SQL 结构：里面的 "(" ")" ";" 以及 \' \\ 转义都不参与计数
	if s.inStr {
		if s.escape {
			s.escape = false
		} else if ch == '\\' {
			s.escape = true
		} else if ch == '\'' {
			s.inStr = false
		}
		return
	}

	// 关键字 / 标识符按「攒到非单词字符为止」收尾
	if isSQLWordChar(ch) {
		if s.phase == sqlPhaseTable {
			s.name += string(ch)
		}
		s.word += string(ch)
		return
	}

	word := strings.ToLower(s.word)
	s.word = ""

	switch s.phase {
	case sqlPhaseIdle:
		if word == "insert" {
			s.phase = sqlPhaseInto
		}
	case sqlPhaseInto:
		if word == "ignore" {
			break // INSERT IGNORE INTO
		}
		if word == "into" {
			s.phase = sqlPhaseTable
			s.name = ""
		} else {
			s.phase = sqlPhaseIdle
		}
	case sqlPhaseTable:
		if ch == '`' {
			s.quoted = !s.quoted
			break
		}
		if s.name != "" {
			s.table = s.name
			s.name = ""
			if _, ok := s.counts[s.table]; !ok {
				s.counts[s.table] = 0
			}
			s.phase = sqlPhaseHead
		}
	case sqlPhaseHead:
		if ch == '`' {
			s.quoted = !s.quoted
			break
		}
		if word == "values" && !s.quoted {
			s.phase = sqlPhaseValues
			s.depth = 0
			// VALUES 与第一个值组可能紧贴着写：VALUES(1),(2)
			s.countValueGroupChar(ch)
		}
	case sqlPhaseValues:
		s.countValueGroupChar(ch)
	}
}

// countValueGroupChar 值组区里的一次字符推进：数深度 0→1 的括号，分号收尾。
func (s *sqlCountState) countValueGroupChar(ch byte) {
	switch {
	case ch == '\'':
		s.inStr = true
	case ch == '(':
		if s.depth == 0 && s.table != "" {
			s.counts[s.table]++
		}
		s.depth++
	case ch == ')':
		if s.depth > 0 {
			s.depth--
		}
	case ch == ';' && s.depth == 0:
		s.phase = sqlPhaseIdle
		s.table = ""
		s.name = ""
		s.quoted = false
	}
}

// parseDumpTableCounts 解析 mysqldump 文件中每个表的行数（通过 INSERT 语句计数）。
func parseDumpTableCounts(sqlPath string, files BackupFileStore) (map[string]int64, error) {
	raw, err := files.ReadFile(sqlPath)
	if err != nil {
		return nil, err
	}
	st := newSQLCountState()
	for i := 0; i < len(raw); i++ {
		st.feedSQLChar(raw[i])
	}
	return st.counts, nil
}

// calculateDiffFromSql 从 SQL dump 解析各表行数差异。
func (d *BackupDomain) calculateDiffFromSql(ctx context.Context, sqlPath string) (jsArr, error) {
	importCounts, err := parseDumpTableCounts(sqlPath, d.files)
	if err != nil {
		return nil, err
	}
	currentCounts, err := d.store.TableCounts(ctx, backupDiffTables)
	if err != nil {
		return nil, err
	}
	diff := make(jsArr, 0, len(backupDiffTables))
	for _, table := range backupDiffTables {
		current := currentCounts[table]
		importCount := importCounts[table]
		newRows := importCount - current
		if newRows < 0 {
			newRows = 0
		}
		diff = append(diff, jo(
			"table", table,
			"current", current,
			"import", importCount,
			"new", newRows,
		))
	}
	return diff, nil
}

// ---- import ----

// backupImport 对齐 POST /admin-api/backup/import。
func (d *BackupDomain) backupImport(c *gin.Context) {
	data := jsonInput(c)
	v := newValidator(ctxOf(c), data)
	v.field("mode", "required", "in:overwrite,merge")
	v.field("restore_env", "nullable", "boolean")
	v.field("site_url", "nullable")
	if !v.passes() {
		if err := v.storeFailure(); err != nil {
			d.failServer(c, "校验导入参数", err)
			return
		}
		d.failValidation(c, v.summary())
		return
	}
	// url 规则单独校验（共享校验器没有 url 规则）
	if val, ok := data["site_url"]; ok && val != nil {
		s, _ := inputString(val)
		if s != "" && !urlValid(s) {
			d.failValidation(c, "The site url field must be a valid URL.")
			return
		}
	}

	tmpDir, err := d.files.TmpDir()
	if err != nil {
		d.failServer(c, "创建临时目录", err)
		return
	}
	backupFile, ext := d.findUploadedBackup(tmpDir)
	if backupFile == "" {
		d.fail(c, "请先上传备份文件", 400)
		return
	}

	// 只认「刚预览过的那一份上传」
	if stale := d.consumePreviewRecord(tmpDir, backupFile); stale != "" {
		d.clearPreviewArtifacts(tmpDir)
		d.fail(c, stale, 400)
		return
	}

	mode, _ := inputString(data["mode"])
	restoreEnv := false
	if val, ok := data["restore_env"]; ok {
		restoreEnv = inputBool(val)
	}
	siteURL := ""
	if val, ok := data["site_url"]; ok && val != nil {
		siteURL, _ = inputString(val)
	}

	d.envNotice = nil

	sqlPath, envNotice, extractErr := d.extractBackupFile(backupFile, ext, restoreEnv)
	if extractErr != nil {
		_ = d.files.Remove(backupFile)
		d.fail(c, "导入失败："+extractErr.Error(), 500)
		return
	}
	if envNotice != "" {
		d.envNotice = &envNotice
	}

	// 用户指定了站点地址才改本机 APP_URL（留空 = 一个字都不动）
	if siteURL != "" {
		d.env.UpdateKey("APP_URL", siteURL)
	}

	var importErr error
	if mode == "overwrite" {
		importErr = d.tools.ImportSQL(ctxOf(c), d.db, sqlPath)
	} else {
		importErr = d.mergeImport(ctxOf(c), sqlPath)
	}
	_ = d.files.Remove(sqlPath)
	_ = d.files.Remove(backupFile)
	if importErr != nil {
		d.fail(c, "导入失败："+importErr.Error(), 500)
		return
	}

	// ===== 数据处理完了，下面是收尾 =====
	notices := []string{}
	failures := []string{}
	var migrateRan *int64
	queueRestarted := false

	if n, err := d.post.RunPendingMigrations(ctxOf(c)); err != nil {
		failures = append(failures, "数据库迁移失败："+err.Error()+"（可手动执行 php artisan migrate --force）")
	} else {
		migrateRan = &[]int64{int64(n)}[0]
		if n > 0 {
			notices = append(notices, fmt.Sprintf("数据库迁移：补跑了 %d 个", n))
		} else {
			notices = append(notices, "数据库迁移：无新迁移")
		}
	}

	if err := d.post.RestartQueueWorkers(ctxOf(c)); err != nil {
		failures = append(failures, "队列重启失败："+err.Error()+"（可手动执行 php artisan queue:restart）")
	} else {
		queueRestarted = true
		notices = append(notices, "队列 worker 已重启（会读到新的 APP_KEY）")
	}

	msg := "已覆盖导入"
	if mode != "overwrite" {
		msg = "已增量合并"
	}
	if d.envNotice != nil {
		msg += "；" + *d.envNotice
	}
	msg += "；" + strings.Join(notices, "；")

	payload := jo(
		"env_notice", d.envNotice,
		"notice", msg,
		"migrate_ran", nullableInt(migrateRan),
		"queue_restarted", queueRestarted,
	)

	if len(failures) > 0 {
		// 数据已经在库里了，这里只报「收尾没做完」+ 怎么办，不假装导入失败
		d.fail(c, "数据已导入，但"+strings.Join(failures, "；")+"。已导入的数据未回滚。", 500)
		return
	}
	d.success(c, payload, msg)
}

// mergeImport 增量合并：先导入临时库，再逐表合并到主库。
func (d *BackupDomain) mergeImport(ctx context.Context, sqlPath string) error {
	tmpDb := fmt.Sprintf("controlhub_merge_tmp_%d", d.now().UTC().Unix())
	if err := d.tools.CreateDatabase(ctx, d.db, tmpDb); err != nil {
		return err
	}
	defer func() {
		if err := d.tools.DropDatabase(ctx, d.db, tmpDb); err != nil {
			d.logger.Printf("Failed to drop temp DB %s: %v", tmpDb, err)
		}
	}()
	tmpCfg := d.db
	tmpCfg.Database = tmpDb
	if err := d.tools.ImportSQL(ctx, tmpCfg, sqlPath); err != nil {
		return fmt.Errorf("临时库导入失败：%w", err)
	}
	// 逐表合并由 store 完成（insert-if-absent 语义见 BackupStore）
	if m, ok := d.store.(BackupMergeStore); ok {
		return m.MergeFromDatabase(ctx, tmpDb)
	}
	return nil
}

// BackupMergeStore 是可选能力：从临时库做 insert-if-absent 合并。
type BackupMergeStore interface {
	MergeFromDatabase(ctx context.Context, sourceDB string) error
}

// urlValid 对齐 Laravel `url` 规则（filter_var FILTER_VALIDATE_URL 的口径）。
func urlValid(s string) bool {
	if s == "" {
		return false
	}
	lower := strings.ToLower(s)
	if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
		return false
	}
	// 至少要有 host 部分
	rest := s
	if i := strings.Index(rest, "://"); i >= 0 {
		rest = rest[i+3:]
	}
	if rest == "" {
		return false
	}
	host := rest
	if i := strings.IndexAny(host, "/?#"); i >= 0 {
		host = host[:i]
	}
	if host == "" {
		return false
	}
	if strings.ContainsAny(host, " \t\n\r") {
		return false
	}
	return true
}
