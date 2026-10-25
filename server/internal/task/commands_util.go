package task

// commands_util.go — commands.go 依赖的文件/HTTP/加密辅助实现。

import (
	"compress/gzip"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// cachePrefixShared 与 collector.cacheKeyPrefix 同口径。
func cachePrefixShared() string {
	// 复刻 config/cache.php 的 prefix（CACHE_PREFIX 或 slug(APP_NAME)+'-cache-'）。
	// 直接读环境变量，避免导入 collector 造成循环。
	if p := strings.TrimSpace(os.Getenv("CACHE_PREFIX")); p != "" {
		return p
	}
	slug := slugifySimple(os.Getenv("APP_NAME"))
	if slug == "" {
		slug = "laravel"
	}
	return slug + "-cache-"
}

// slugifySimple 复刻 Str::slug() 的常见场景。
func slugifySimple(s string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// ensureDirImpl 创建目录（含父目录）。
func ensureDirImpl(dir string) error {
	return os.MkdirAll(dir, 0755)
}

// removeIfExistsImpl 删除文件（忽略不存在）。
func removeIfExistsImpl(path string) {
	_ = os.Remove(path)
}

// fileSizeImpl 返回文件大小（不存在返回 0）。
func fileSizeImpl(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}

// pruneOldBackupsImpl 按文件名倒序只保留最新 keep 份。
func pruneOldBackupsImpl(dir string, keep int) {
	pattern := filepath.Join(dir, "backup-*.sql.gz")
	files, err := filepath.Glob(pattern)
	if err != nil || len(files) <= keep {
		return
	}
	sort.Strings(files) // 升序：最旧在前
	expired := files[:len(files)-keep]
	for _, f := range expired {
		_ = os.Remove(f)
	}
}

// gzipFileWriter 是 gzip 文件写入器。
type gzipFileWriter struct {
	f  *os.File
	gz *gzip.Writer
}

func (w *gzipFileWriter) Write(p []byte) (int, error) { return w.gz.Write(p) }

func (w *gzipFileWriter) Close() error {
	err1 := w.gz.Close()
	err2 := w.f.Close()
	if err1 != nil {
		return err1
	}
	return err2
}

// newGzipFileImpl 创建 gzip 文件写入器。
func newGzipFileImpl(path string) (gzipWriteCloser, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	return &gzipFileWriter{f: f, gz: gzip.NewWriter(f)}, nil
}

// httpGetImpl 发起 HTTP GET 请求。
func httpGetImpl(ctx context.Context, url string, timeout time.Duration) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 10*1024*1024))
}

// decodeBase64Impl base64 解码（支持 RawStdEncoding 和 StdEncoding）。
func decodeBase64Impl(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	// 尝试标准 base64
	if decoded, err := base64.StdEncoding.DecodeString(s); err == nil {
		return decoded, nil
	}
	// 尝试 RawStdEncoding（无 padding）
	if decoded, err := base64.RawStdEncoding.DecodeString(s); err == nil {
		return decoded, nil
	}
	// 尝试 URL-safe
	if decoded, err := base64.URLEncoding.DecodeString(s); err == nil {
		return decoded, nil
	}
	if decoded, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return decoded, nil
	}
	return nil, fmt.Errorf("base64 解码失败")
}

// cryptoSHA1Impl 计算 SHA1 hex。
func cryptoSHA1Impl(s string) string {
	h := sha1.Sum([]byte(s))
	return hex.EncodeToString(h[:])
}
