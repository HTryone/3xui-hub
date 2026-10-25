package admin

import "os"

// osGetenv 的默认实现（测试可替换以固定业务时区）。
func init() {
	if osGetenv == nil {
		osGetenv = os.Getenv
	}
}
