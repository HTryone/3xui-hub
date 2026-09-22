<?php

namespace App\Services\ThreeXUi;

/**
 * cookie 模式会话失效（业务请求回 401/403）的内部信号。
 *
 * 只在 ThreeXUiClient::execute() → send() 之间传递：send() 捕获它做
 * 「清缓存 + 重新登录一次 + 只重试该请求一次」。**它不会逃出 send()** ——
 * 重登后的第二次 401 会退化成普通 ThreeXUiException，
 * 免得被 requestNullable() 的 catch (ThreeXUiException) 当成 not-found 吞掉。
 */
class PanelAuthExpiredException extends ThreeXUiException
{
}
