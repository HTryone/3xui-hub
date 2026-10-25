package admin

// backup_os.go 是 BackupTools / BackupZip / BackupFileStore 的生产实现。
// 对齐 PHP 的 mysqldump / mysql / ZipArchive / File::* 调用。

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

// ---- BackupTools：mysqldump / mysql ----

// OSBackupTools 用宿主机 CLI 工具做数据库导出/导入。
type OSBackupTools struct{}

// NewOSBackupTools 构造生产实现。
func NewOSBackupTools() *OSBackupTools { return &OSBackupTools{} }

// ExportDump 跑 mysqldump 把整库 dump 到 destPath。
func (t *OSBackupTools) ExportDump(ctx context.Context, cfg BackupDBConfig, destPath string) error {
	cmd := exec.CommandContext(ctx, "mysqldump",
		"-h", cfg.Host, "-P", cfg.Port,
		"-u", cfg.Username,
		"--single-transaction", "--routines", "--triggers",
		cfg.Database,
	)
	if cfg.Password != "" {
		cmd.Env = append(os.Environ(), "MYSQL_PWD="+cfg.Password)
	}
	f, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("创建 dump 文件失败: %w", err)
	}
	defer f.Close()
	cmd.Stdout = f
	cmd.Stderr = nil
	return cmd.Run()
}

// ImportSQL 把 sqlPath 灌进 cfg.Database。
func (t *OSBackupTools) ImportSQL(ctx context.Context, cfg BackupDBConfig, sqlPath string) error {
	cmd := exec.CommandContext(ctx, "mysql",
		"-h", cfg.Host, "-P", cfg.Port,
		"-u", cfg.Username,
		cfg.Database,
	)
	if cfg.Password != "" {
		cmd.Env = append(os.Environ(), "MYSQL_PWD="+cfg.Password)
	}
	f, err := os.Open(sqlPath)
	if err != nil {
		return fmt.Errorf("打开 SQL 文件失败: %w", err)
	}
	defer f.Close()
	cmd.Stdin = f
	cmd.Stderr = nil
	return cmd.Run()
}

// CreateDatabase 创建临时库（merge 模式用）。
func (t *OSBackupTools) CreateDatabase(ctx context.Context, cfg BackupDBConfig, name string) error {
	cmd := exec.CommandContext(ctx, "mysql",
		"-h", cfg.Host, "-P", cfg.Port,
		"-u", cfg.Username,
		"-e", fmt.Sprintf("CREATE DATABASE IF NOT EXISTS `%s`", name),
	)
	if cfg.Password != "" {
		cmd.Env = append(os.Environ(), "MYSQL_PWD="+cfg.Password)
	}
	return cmd.Run()
}

// DropDatabase 删除临时库。
func (t *OSBackupTools) DropDatabase(ctx context.Context, cfg BackupDBConfig, name string) error {
	cmd := exec.CommandContext(ctx, "mysql",
		"-h", cfg.Host, "-P", cfg.Port,
		"-u", cfg.Username,
		"-e", fmt.Sprintf("DROP DATABASE IF EXISTS `%s`", name),
	)
	if cfg.Password != "" {
		cmd.Env = append(os.Environ(), "MYSQL_PWD="+cfg.Password)
	}
	return cmd.Run()
}

// ---- BackupZip：archive/zip ----

// OSBackupZip 用 Go 标准库做 zip 打包/解包。
type OSBackupZip struct{}

// NewOSBackupZip 构造生产实现。
func NewOSBackupZip() *OSBackupZip { return &OSBackupZip{} }

// Build 打包 dump.sql + 可选 .env。
func (z *OSBackupZip) Build(dumpPath, zipPath, envContents string) error {
	out, err := os.Create(zipPath)
	if err != nil {
		return err
	}
	defer out.Close()

	w := zip.NewWriter(out)
	defer w.Close()

	// dump.sql
	dumpFile, err := os.Open(dumpPath)
	if err != nil {
		return err
	}
	defer dumpFile.Close()

	dumpEntry, err := w.Create("dump.sql")
	if err != nil {
		return err
	}
	if _, err := io.Copy(dumpEntry, dumpFile); err != nil {
		return err
	}

	// .env（可选）
	if envContents != "" {
		envEntry, err := w.Create(".env")
		if err != nil {
			return err
		}
		if _, err := envEntry.Write([]byte(envContents)); err != nil {
			return err
		}
	}
	return nil
}

// ExtractSQL 解出 dump.sql 到 destSQL；返回 .env 原文。
func (z *OSBackupZip) ExtractSQL(zipPath, destSQL string) (string, bool, error) {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return "", false, err
	}
	defer r.Close()

	var envContents string
	hasEnv := false

	for _, f := range r.File {
		switch f.Name {
		case "dump.sql":
			rc, err := f.Open()
			if err != nil {
				return "", false, err
			}
			out, err := os.Create(destSQL)
			if err != nil {
				rc.Close()
				return "", false, err
			}
			_, err = io.Copy(out, rc)
			out.Close()
			rc.Close()
			if err != nil {
				return "", false, err
			}
		case ".env":
			rc, err := f.Open()
			if err != nil {
				return "", false, err
			}
			data, err := io.ReadAll(rc)
			rc.Close()
			if err != nil {
				return "", false, err
			}
			envContents = string(data)
			hasEnv = true
		}
	}
	return envContents, hasEnv, nil
}

// HasEnv 判断 zip 里有没有 .env。
func (z *OSBackupZip) HasEnv(zipPath string) bool {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return false
	}
	defer r.Close()
	for _, f := range r.File {
		if f.Name == ".env" {
			return true
		}
	}
	return false
}

// ReadEnv 读 zip 里 .env 原文。
func (z *OSBackupZip) ReadEnv(zipPath string) (string, bool, error) {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return "", false, err
	}
	defer r.Close()
	for _, f := range r.File {
		if f.Name == ".env" {
			rc, err := f.Open()
			if err != nil {
				return "", false, err
			}
			data, err := io.ReadAll(rc)
			rc.Close()
			if err != nil {
				return "", false, err
			}
			return string(data), true, nil
		}
	}
	return "", false, nil
}

// ---- BackupFileStore：磁盘操作 ----

// OSBackupFiles 是生产文件操作实现。
type OSBackupFiles struct {
	tmpDir string
}

// NewOSBackupFiles 构造生产实现（默认用系统临时目录下的 controlhub-backup）。
func NewOSBackupFiles() *OSBackupFiles {
	return &OSBackupFiles{tmpDir: filepath.Join(os.TempDir(), "controlhub-backup")}
}

// TmpDir 返回（并创建）备份临时目录。
func (s *OSBackupFiles) TmpDir() (string, error) {
	if err := os.MkdirAll(s.tmpDir, 0o755); err != nil {
		return "", err
	}
	return s.tmpDir, nil
}

// WriteFile 写文件。
func (s *OSBackupFiles) WriteFile(path string, data []byte) error {
	return os.WriteFile(path, data, 0o644)
}

// ReadFile 读文件。
func (s *OSBackupFiles) ReadFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

// Remove 删除文件。
func (s *OSBackupFiles) Remove(path string) error {
	return os.Remove(path)
}

// Exists 判断文件是否存在。
func (s *OSBackupFiles) Exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Size 返回文件大小。
func (s *OSBackupFiles) Size(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}

// SHA256 计算文件哈希。
func (s *OSBackupFiles) SHA256(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// CopyFile 复制文件。
func (s *OSBackupFiles) CopyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
