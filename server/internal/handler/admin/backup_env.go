package admin

// BackupEnvService 的 Go 复刻（backend/app/Services/BackupEnvService.php）。
//
// 只搬 APP_KEY 这一行：节点凭据 / 邮件 / 支付密码都是 Crypt::encryptString 加密存的，
// 跨机迁移必须带上 APP_KEY。其余各行（DB 密码、QUEUE_CONNECTION…）都是本机自己的，
// 整份覆盖会把它们冲掉 —— 历史上发生过导入后 QUEUE_CONNECTION 变 sync 的事故。

import (
	"strings"
)

// backupEnvServerKeys 是导入预览里展示差异的服务器相关配置项（保序）。
var backupEnvServerKeys = []string{
	"APP_NAME", "APP_URL", "APP_KEY", "APP_DEBUG", "APP_ENV",
	"DB_CONNECTION", "DB_HOST", "DB_PORT", "DB_DATABASE",
	"SESSION_DRIVER", "SESSION_DOMAIN",
	"MAIL_MAILER", "MAIL_HOST", "MAIL_PORT", "MAIL_FROM_ADDRESS",
	"REDIS_HOST", "REDIS_PORT",
}

// backupEnvSensitiveKeys 是预览里不回传明文、只标记「不同」的配置项。
var backupEnvSensitiveKeys = map[string]bool{"APP_KEY": true}

// BackupEnvService 处理备份文件里的 .env。
type BackupEnvService struct {
	envPath string
	files   BackupFileStore
	logger  loggerLike
}

// NewBackupEnvService 构造 .env 处理器。
func NewBackupEnvService(envPath string, files BackupFileStore, logger loggerLike) *BackupEnvService {
	return &BackupEnvService{envPath: envPath, files: files, logger: logger}
}

// EnvPath 本机 .env 路径。
func (s *BackupEnvService) EnvPath() string { return s.envPath }

// AppKeyOnlyContents 导出用：备份 zip 里 .env 条目的内容，只有 APP_KEY 一行。
// 本机没有 APP_KEY 时返回 ""（zip 里不写 .env 条目）。
func (s *BackupEnvService) AppKeyOnlyContents() string {
	appKey := s.ParseEnv(s.envPath)["APP_KEY"]
	if appKey == "" {
		return ""
	}
	return "APP_KEY=" + appKey + "\n"
}

// RestoreAppKeyFromEnvContents 导入用：把备份里的 .env 原文交给 restoreAppKey。
// 返回需要展示给用户的提示；空串表示已正常恢复。
func (s *BackupEnvService) RestoreAppKeyFromEnvContents(backupEnvContents *string) string {
	if backupEnvContents == nil {
		s.logger.Printf("Backup import: 备份里没有 .env，跳过 APP_KEY 恢复")
		return "备份里没有 .env，已跳过 APP_KEY 恢复（本机 .env 未改动）"
	}
	return s.restoreAppKey(ParseEnvContent(*backupEnvContents)["APP_KEY"])
}

func (s *BackupEnvService) restoreAppKey(appKey string) string {
	if appKey == "" {
		s.logger.Printf("Backup import: 备份里没有 APP_KEY，跳过 .env 恢复")
		return "备份里没有 APP_KEY，已跳过（本机 .env 未改动）"
	}
	if !s.files.Exists(s.envPath) {
		s.logger.Printf("Backup import: 本机 .env 不存在，APP_KEY 未写入")
		return "本机 .env 不存在，APP_KEY 未写入"
	}
	// 覆盖前留一份，便于人工回退
	_ = s.files.CopyFile(s.envPath, s.envPath+".bak")
	s.UpdateKey("APP_KEY", appKey)
	s.logger.Printf("Backup import: APP_KEY 已恢复（.env 其余行未改动）")
	return ""
}

// UpdateKey 更新本机 .env 中某个 key 的值：只改这一行，没有该行才追加，其余行不动。
func (s *BackupEnvService) UpdateKey(key, value string) {
	if !s.files.Exists(s.envPath) {
		return
	}
	raw, err := s.files.ReadFile(s.envPath)
	if err != nil {
		return
	}
	content := string(raw)
	replacement := key + "=" + value
	lines := strings.Split(content, "\n")
	found := false
	for i, line := range lines {
		trimmed := strings.TrimRight(line, "\r")
		if strings.HasPrefix(trimmed, key+"=") {
			lines[i] = replacement
			found = true
			// PHP preg_replace /m 会替换所有匹配行；这里与之一致，不 break
		}
	}
	out := strings.Join(lines, "\n")
	if !found {
		out = strings.TrimRight(content, "\r\n") + "\n" + replacement + "\n"
	}
	_ = s.files.WriteFile(s.envPath, []byte(out))
	// 只记 key 不记值：APP_KEY 这类敏感值不该进日志
	s.logger.Printf("Backup import: %s updated", key)
}

// EnvDiff 预览用：备份 .env 与本机 .env 的差异（仅展示，导入时只有 APP_KEY 会生效）。
//
// 对齐 PHP：备份里没有的键跳过；备份值 === 当前值（含两边都空串）跳过；
// 当前没设过的键展示「（未设置）」。
func (s *BackupEnvService) EnvDiff(backupEnv map[string]string) jsArr {
	currentEnv := s.ParseEnv(s.envPath)
	out := make(jsArr, 0, 4)
	for _, key := range backupEnvServerKeys {
		backupVal, ok := backupEnv[key]
		if !ok {
			continue
		}
		currentVal, has := currentEnv[key]
		if has && backupVal == currentVal {
			continue
		}
		masked := backupEnvSensitiveKeys[key]
		currentOut := any(currentVal)
		backupOut := any(backupVal)
		if masked {
			currentOut = "不同"
			backupOut = "不同"
		} else if !has {
			currentOut = "（未设置）"
		}
		out = append(out, jo("key", key, "current", currentOut, "backup", backupOut))
	}
	return out
}

// ParseEnv 解析 .env 文件为键值数组。
func (s *BackupEnvService) ParseEnv(path string) map[string]string {
	if !s.files.Exists(path) {
		return map[string]string{}
	}
	raw, err := s.files.ReadFile(path)
	if err != nil {
		return map[string]string{}
	}
	return ParseEnvContent(string(raw))
}

// ParseEnvContent 解析 .env 文本为键值数组（对齐 PHP preg_split(/\r\n|\n|\r/) + trim）。
func ParseEnvContent(content string) map[string]string {
	result := map[string]string{}
	for _, line := range splitEnvLines(content) {
		line = strings.Trim(line, " \t\n\r\x00\x0B")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.Trim(parts[1], " \t\n\r\x00\x0B\"")
		result[key] = val
	}
	return result
}

// splitEnvLines 按 \r\n | \n | \r 切行。
func splitEnvLines(content string) []string {
	var out []string
	start := 0
	for i := 0; i < len(content); i++ {
		switch content[i] {
		case '\r':
			if i+1 < len(content) && content[i+1] == '\n' {
				out = append(out, content[start:i])
				i++
				start = i + 1
			} else {
				out = append(out, content[start:i])
				start = i + 1
			}
		case '\n':
			out = append(out, content[start:i])
			start = i + 1
		}
	}
	if start <= len(content) {
		out = append(out, content[start:])
	}
	return out
}
