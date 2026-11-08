package admin

// backup_settings_test.go 锁死「自动备份」开关的两条关键行为：
//  1. 缺省（未配置键）= 开启；
//  2. 关值（0/false/off/no）= 关闭；
//  3. 无论开关如何，手动导出/导入都不受它影响 —— 这条靠「backupExport/backupPreview/backupImport
//     里没有任何 backupAutoEnabled 判断」来保证，这里用源码扫描做回归护栏（防止以后有人又把它接回去）。

import (
	"context"
	"os"
	"strings"
	"testing"
)

// fakeBackupSettings 是内存版 site_configs（只实现备份开关需要的两个方法）。
type fakeBackupSettings struct {
	kv map[string]string
}

func (f *fakeBackupSettings) SiteConfigMany(_ context.Context, keys []string) (map[string]string, error) {
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		out[k] = f.kv[k]
	}
	return out, nil
}

func (f *fakeBackupSettings) SiteConfigSetMany(_ context.Context, kv []KV) error {
	for _, item := range kv {
		if item.Value == nil {
			delete(f.kv, item.Key)
			continue
		}
		f.kv[item.Key] = *item.Value
	}
	return nil
}

func TestBackupAutoEnabled_DefaultOn(t *testing.T) {
	d := &BackupDomain{settings: &fakeBackupSettings{kv: map[string]string{}}}
	on, err := d.backupAutoEnabled(context.Background())
	if err != nil {
		t.Fatalf("意外错误: %v", err)
	}
	if !on {
		t.Fatal("键不存在时应默认开启")
	}
}

func TestBackupAutoEnabled_OffValues(t *testing.T) {
	for _, v := range []string{"0", "false", "off", "no", " OFF "} {
		d := &BackupDomain{settings: &fakeBackupSettings{kv: map[string]string{backupAutoEnabledKey: v}}}
		on, err := d.backupAutoEnabled(context.Background())
		if err != nil {
			t.Fatalf("%q: 意外错误 %v", v, err)
		}
		if on {
			t.Errorf("%q 应判为关闭", v)
		}
	}
}

func TestBackupAutoEnabled_OnValues(t *testing.T) {
	for _, v := range []string{"1", "true", "on", "yes", "anything"} {
		d := &BackupDomain{settings: &fakeBackupSettings{kv: map[string]string{backupAutoEnabledKey: v}}}
		on, err := d.backupAutoEnabled(context.Background())
		if err != nil {
			t.Fatalf("%q: 意外错误 %v", v, err)
		}
		if !on {
			t.Errorf("%q 应判为开启", v)
		}
	}
}

// TestBackupHandlers_NotGatedByAutoEnabled 是回归护栏：
// 用户明确要求「手动导出/导入与定时任务无关」，三个 handler 里不允许出现开关判断。
// 以后若有人误把它接回去，本测试会失败。
func TestBackupHandlers_NotGatedByAutoEnabled(t *testing.T) {
	raw, err := os.ReadFile("backup.go")
	if err != nil {
		t.Skipf("读不到 backup.go（打包/分发环境跳过）: %v", err)
	}
	src := string(raw)

	// 逐个函数体粗切：找到函数名到下一个顶层 func 之间，断言不含 backupAutoEnabled。
	funcs := []string{
		"func (d *BackupDomain) backupExport(",
		"func (d *BackupDomain) backupPreview(",
		"func (d *BackupDomain) backupImport(",
	}
	for _, sig := range funcs {
		idx := strings.Index(src, sig)
		if idx < 0 {
			t.Fatalf("找不到函数签名 %q（代码结构变了，请更新测试）", sig)
		}
		rest := src[idx+len(sig):]
		if next := strings.Index(rest, "\nfunc "); next >= 0 {
			rest = rest[:next]
		}
		if strings.Contains(rest, "backupAutoEnabled") {
			t.Errorf("%s 里出现了 backupAutoEnabled —— 手动导出/导入不应受「自动备份」开关影响", sig)
		}
	}
}
