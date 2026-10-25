package threexui

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// 错误类型逐条对齐 backend/app/Services/ThreeXUi/*Exception.php。
//
// 为什么要分四类而不是一个 error：调用方对它们的反应完全不同 ——
//   - Error          面板明确报错（success=false）或响应结构非法 → 照原样上抛 / 记日志
//   - ClientNotFound 该 email 在面板上不存在 → 订阅少一个链接、批量操作跳过该用户
//   - AuthExpired    cookie 模式登录态被顶掉 → 清缓存重登一次再试
//   - TransportError 网络层失败（连不上/超时/5xx） → 必须重试或标节点离线，绝不能降级
//
// TransportError **不是** Error 的子类：PHP 侧 Guzzle 的 ConnectException 继承
// TransferException 而非 RequestException，正因如此 requestNullable() 的
// `catch (ThreeXUiException)` 接不住它，「查不到就返回 null」的降级不会吞掉网络故障
// （见 tests/Unit/ThreeXUiClientTest::test_get_client_links_still_throws_on_network_error）。
// Go 侧用两个不相关的类型保住这条边界。

// Error 是 3x-ui 业务异常（对应 ThreeXUiException）。
type Error struct {
	Message string
	// Status 是 HTTP 状态码（0 表示还没拿到响应）。
	Status int
}

func (e *Error) Error() string { return e.Message }

// ClientNotFoundError 表示「该 email 在面板上不存在」（对应 ClientNotFoundException）。
type ClientNotFoundError struct{ Message string }

func (e *ClientNotFoundError) Error() string { return e.Message }

// Unwrap 让 errors.As(err, *Error) 也能命中（PHP 里它是 ThreeXUiException 子类）。
func (e *ClientNotFoundError) Unwrap() error { return &Error{Message: e.Message} }

// AuthExpiredError 是 cookie 模式会话失效的内部信号（对应 PanelAuthExpiredException）。
// 只会出现在 send() 内部，不外泄。
type AuthExpiredError struct{ Message string }

func (e *AuthExpiredError) Error() string { return e.Message }

// Unwrap 同上。
func (e *AuthExpiredError) Unwrap() error { return &Error{Message: e.Message} }

// TransportError 是网络层失败（连接超时/拒绝/DNS/5xx 之外的传输错误）。
type TransportError struct{ Err error }

func (e *TransportError) Error() string { return e.Err.Error() }
func (e *TransportError) Unwrap() error { return e.Err }

// 判定错误「是不是面板说 client 不存在」。
//
// 3x-ui 对不存在的 email 返回 HTTP 200 + {success:false, msg:"Obtain (record not found)"}，
// 与普通业务失败的响应结构完全一致，只能按文案识别；各版本措辞不一，故用关键字兜底。
// 带传输层前缀的异常一律不算 not-found（PHP 侧同一张前缀黑名单）。
var (
	notFoundPrefixes = []string{
		"3x-ui request failed:",
		"3x-ui login failed:",
		"invalid 3x-ui response:",
	}
	notFoundPattern = regexp.MustCompile(`(?i)record not found|not found|not exist|不存在`)
)

// IsClientNotFound 对应 ThreeXUiClient::isClientNotFound()。
func IsClientNotFound(err error) bool {
	if err == nil {
		return false
	}
	var nf *ClientNotFoundError
	if errors.As(err, &nf) {
		return true
	}
	msg := err.Error()
	for _, prefix := range notFoundPrefixes {
		if strings.HasPrefix(msg, prefix) {
			return false
		}
	}
	return notFoundPattern.MatchString(msg)
}

// AsNotFound 把「文案判定为不存在」的业务错误包成 ClientNotFoundError（供调用方 errors.As）。
func asNotFound(err error) error {
	if err == nil {
		return nil
	}
	var nf *ClientNotFoundError
	if errors.As(err, &nf) {
		return err
	}
	var bizErr *Error
	if errors.As(err, &bizErr) && IsClientNotFound(bizErr) {
		return &ClientNotFoundError{Message: bizErr.Message}
	}
	return err
}

// invalidResponse 对应 `new ThreeXUiException('invalid 3x-ui response: ' . substr($body, 0, 200))`。
func invalidResponse(body []byte) *Error {
	return &Error{Message: "invalid 3x-ui response: " + firstBytes(body, 200)}
}

// firstBytes 取前 n 个**字节**（PHP substr 是字节语义，中文可能被截半）。
func firstBytes(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n])
}

// requestFailed 对应 '3x-ui request failed: ' . $e->getMessage()。
func requestFailed(err error) *Error {
	return &Error{Message: "3x-ui request failed: " + err.Error()}
}

// loginFailed 对应 '3x-ui login failed: ' . msg。
func loginFailed(msg string) *Error {
	if msg == "" {
		msg = "invalid credentials"
	}
	return &Error{Message: "3x-ui login failed: " + msg}
}

// loginTransportFailed 对应 loginErrorMessage()：
// 连接类失败翻译成人话（跨境节点最常见的就是 host/端口打不通或超时）。
//
// Go 的 http.Client 不细分 ConnectException，故把 login 阶段的所有传输错误都归到这里 ——
// 这一阶段除了「连不上」不可能有别的传输错误。
func loginTransportFailed(err error) *Error {
	return &Error{Message: fmt.Sprintf(
		"3x-ui login failed: 节点登录超时，请检查 host/端口是否可达，或改用 api_key（%s）", err.Error())}
}
