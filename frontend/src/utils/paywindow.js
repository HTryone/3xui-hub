/**
 * 支付新窗口的「同步预占」工具（仅 iOS 需要，其他平台行为完全不变）。
 *
 * 背景（iOS Safari 坑）：WebKit 的「用户手势激活」（User Gesture Activation）策略会
 * **静默拦截**任何不在点击同步调用栈内的 window.open。而本项目所有支付入口都是：
 *   点按钮 → await 请求拿 pay_url → window.open(pay_url, '_blank')
 * 等 await 回来时手势上下文已经丢了 → iOS Safari 直接把新窗口拦掉，
 * 用户只看到「订单已创建」，收银台/二维码根本不弹（设置里关掉「阻止弹窗」也没用）。
 *
 * 解法：iOS 上在点击的**同步**阶段先占一个空白窗口（这一步在手势栈内，不会被拦），
 * 异步拿到链接后再把这个已存在的窗口导航过去（导航到跨域地址是允许的）。
 *
 * 兼容性保证：**只有 iOS（含 iPadOS 桌面模式）才预占窗口**；桌面 / Android / 其它浏览器
 * 走 `gotoPay` 时仍是原来的 `window.open(url, '_blank')`，行为、观感一字不变。
 *
 * 用法（点击处理函数里）：
 *   const payWin = beginPayWindow()          // 同步、且必须在任何 await 之前
 *   ...
 *   const result = await createOrder()
 *   if (result.pay_url) gotoPay(payWin, result.pay_url)
 *   else cancelPayWindow(payWin)             // 失败时别把空白页留给用户
 */

// 是否 iOS 系（含 iPadOS 13+ 伪装成 Mac 的桌面模式）。
// 这些环境下的浏览器内核一律是 WebKit，对「异步 window.open」拦截最狠。
export function isIOSWebKit() {
  if (typeof navigator === 'undefined') return false
  const ua = navigator.userAgent || ''
  if (/iPad|iPhone|iPod/.test(ua)) return true
  // iPadOS 13+「请求桌面网站」时 UA 伪装成 Mac：用「Mac 平台 + 多点触控」识别
  if (navigator.platform === 'MacIntel' && (navigator.maxTouchPoints || 0) > 1) return true
  return false
}

// 开始一次支付：iOS 才同步预占一个空白窗口；其它平台返回 { iOS:false, handle:null }，
// 后续 gotoPay 会走原生的 window.open（即改动前的行为）。
// 必须在点击事件的同步执行栈里调用（await 之前）。
export function beginPayWindow() {
  const iOS = isIOSWebKit()
  let handle = null
  if (iOS) {
    try {
      // 不传 noopener —— 那样拿不到句柄，后续没法导航它
      handle = window.open('', '_blank')
    } catch (e) {
      handle = null
    }
  }
  return { iOS, handle }
}

// 把支付链接送到正确的地方：
//   - iOS 且有预占窗口 → 导航那个窗口去支付链接
//   - iOS 但预占被拦 → 当前页跳转（Safari 对当前页导航最宽松，付完按 return_url 回来）
//   - 非 iOS → 与改动前完全一致：window.open(url, '_blank')
export function gotoPay(state, url) {
  if (!url) return
  if (state && state.iOS) {
    if (state.handle && !state.handle.closed) {
      try {
        state.handle.location.href = url
        return
      } catch (e) {
        /* 窗口已被关闭/无法访问：走下面的当前页跳转兜底 */
      }
    }
    window.location.href = url
    return
  }
  window.open(url, '_blank')
}

// 预占了窗口但最终没拿到支付链接（或走了「已支付」直接返回）时，把空白页关掉，
// 避免给用户留一个空标签页。非 iOS 没预占，handle 为空，直接忽略。
export function cancelPayWindow(state) {
  if (state && state.handle && !state.handle.closed) {
    try {
      state.handle.close()
    } catch (e) {
      /* 关不掉就算了 */
    }
  }
}
