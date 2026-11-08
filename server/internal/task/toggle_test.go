package task

// toggle_test.go 锁死「统一定时任务开关」的判定口径（缺省开、关值识别）。
// 这几个行为一旦被改坏，备份开关/系统状态页隐藏都会跟着错，必须有测试兜底。

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/go-sql-driver/mysql"
)

func TestTaskToggleKey(t *testing.T) {
	if got := TaskToggleKey("db-backup"); got != "sched_db-backup_enabled" {
		t.Fatalf("TaskToggleKey(db-backup) = %q, want sched_db-backup_enabled", got)
	}
}

func TestIsOffValue(t *testing.T) {
	off := []string{"0", "false", "FALSE", "off", "Off", "no", " 0 ", "off "}
	for _, v := range off {
		if !isOffValue(v) {
			t.Errorf("isOffValue(%q) = false, want true（应判为关闭）", v)
		}
	}
	on := []string{"1", "true", "on", "yes", "", "anything", "2"}
	for _, v := range on {
		if isOffValue(v) {
			t.Errorf("isOffValue(%q) = true, want false（应判为开启）", v)
		}
	}
}

// TestConditionChecker_UnconditionalAlwaysTrue：没有条件的任务必须恒 true（永不隐藏）。
func TestConditionChecker_UnconditionalAlwaysTrue(t *testing.T) {
	chk := ConditionChecker(nil)
	for _, name := range []string{NameTrafficSync, NameBanCheck, NameHealthCheck, NameDBBackup} {
		ok, err := chk(context.Background(), name)
		if err != nil || !ok {
			t.Errorf("无条件任务 %s 应恒 true，得到 ok=%v err=%v", name, ok, err)
		}
	}
	// nil lookups 时，即使是有条件的任务也按 true（不隐藏），不 panic。
	if ok, err := chk(context.Background(), NameXrayWarpRotate); err != nil || !ok {
		t.Errorf("nil lookups 下 WARP 条件应保守返回 true，得到 ok=%v err=%v", ok, err)
	}
}

// fakeLookups 用来验证 ConditionChecker 的分派是否指对了方法。
type fakeLookups struct {
	called string
	val    bool
}

func (f *fakeLookups) MailNotifyAnyEnabled(context.Context) (bool, error) {
	f.called = NameMailNotifyScan
	return f.val, nil
}
func (f *fakeLookups) MailScheduleEnabled(context.Context) (bool, error) {
	f.called = NameMailScheduleScan
	return f.val, nil
}
func (f *fakeLookups) ThirdPartyFetchEnabled(context.Context) (bool, error) {
	f.called = NameThirdPartyFetch
	return f.val, nil
}
func (f *fakeLookups) WarpRotateEnabled(context.Context) (bool, error) {
	f.called = NameXrayWarpRotate
	return f.val, nil
}
func (f *fakeLookups) OutboundSubsEnabled(context.Context) (bool, error) {
	f.called = NameXrayRefreshOutbound
	return f.val, nil
}

// TestConditionChecker_Dispatch：每条带条件任务必须分派到自己的条件方法，值原样透传。
func TestConditionChecker_Dispatch(t *testing.T) {
	names := []string{
		NameMailNotifyScan, NameMailScheduleScan, NameThirdPartyFetch,
		NameXrayWarpRotate, NameXrayRefreshOutbound,
	}
	for _, name := range names {
		f := &fakeLookups{val: false}
		chk := ConditionChecker(f)
		ok, err := chk(context.Background(), name)
		if err != nil {
			t.Fatalf("%s: 意外错误 %v", name, err)
		}
		if f.called != name {
			t.Errorf("%s: 分派到了 %q，说明条件映射错位", name, f.called)
		}
		if ok {
			t.Errorf("%s: 底层返回 false，ConditionChecker 应返回 false（触发隐藏）", name)
		}
	}
}

// TestNewSQLToggle_NilDB 只验构造不 panic；真实读库行为由集成环境验证。
func TestNewSQLToggle_NilDB(t *testing.T) {
	var db *sql.DB // nil，仅确保函数可被构造
	fn := NewSQLToggle(db)
	if fn == nil {
		t.Fatal("NewSQLToggle 返回 nil")
	}
}
