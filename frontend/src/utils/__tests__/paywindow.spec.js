import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { beginPayWindow, gotoPay, cancelPayWindow, isIOSWebKit } from '@/utils/paywindow'

/**
 * paywindow 的兼容性红线：**只有 iOS 预占空白窗口**，其它平台 gotoPay 必须与
 * 改动前完全一致的 window.open(url, '_blank')。这条一旦被改回「所有平台都预占」，
 * 桌面/安卓的支付观感会变（多一个空白页闪烁），所以用单测锁死。
 */

const IOS_UA =
  'Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1'
const DESKTOP_UA =
  'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36'

function setUA(ua, platform = '', maxTouchPoints = 0) {
  vi.stubGlobal('navigator', { userAgent: ua, platform, maxTouchPoints })
}

// 一个可控的假窗口对象
function fakeWin() {
  return { closed: false, location: { href: '' }, close: vi.fn() }
}

let openSpy
let locHref

beforeEach(() => {
  openSpy = vi.fn()
  locHref = { value: '' }
  vi.stubGlobal('window', {
    open: (...args) => {
      openSpy(...args)
      return openSpy.mock.results.at(-1)?.value ?? null
    },
    get location() {
      return {
        get href() {
          return locHref.value
        },
        set href(v) {
          locHref.value = v
        },
      }
    },
  })
})

afterEach(() => {
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
})

describe('isIOSWebKit', () => {
  it('识别 iPhone/iPad/iPod', () => {
    setUA(IOS_UA)
    expect(isIOSWebKit()).toBe(true)
  })
  it('识别 iPadOS 桌面模式（MacIntel + 多点触控）', () => {
    setUA('Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15) AppleWebKit/605.1.15', 'MacIntel', 5)
    expect(isIOSWebKit()).toBe(true)
  })
  it('桌面 Chrome 不算 iOS', () => {
    setUA(DESKTOP_UA, 'Win32', 0)
    expect(isIOSWebKit()).toBe(false)
  })
})

describe('非 iOS：行为与改动前一致', () => {
  it('beginPayWindow 不预占窗口', () => {
    setUA(DESKTOP_UA, 'Win32', 0)
    const st = beginPayWindow()
    expect(st.iOS).toBe(false)
    expect(st.handle).toBeNull()
    expect(openSpy).not.toHaveBeenCalled()
  })
  it('gotoPay 直接 window.open(url, _blank)，不做 location 跳转', () => {
    setUA(DESKTOP_UA, 'Win32', 0)
    const st = beginPayWindow()
    gotoPay(st, 'https://pay.example.com/x')
    expect(openSpy).toHaveBeenCalledWith('https://pay.example.com/x', '_blank')
    expect(locHref.value).toBe('')
  })
})

describe('iOS：同步预占 + 导航', () => {
  it('beginPayWindow 同步打开空白占位窗', () => {
    setUA(IOS_UA)
    const w = fakeWin()
    openSpy.mockReturnValueOnce(w)
    const st = beginPayWindow()
    expect(st.iOS).toBe(true)
    expect(st.handle).toBe(w)
    expect(openSpy).toHaveBeenCalledWith('', '_blank')
  })
  it('gotoPay 导航到预占窗口，而不是再开新窗', () => {
    setUA(IOS_UA)
    const w = fakeWin()
    openSpy.mockReturnValueOnce(w)
    const st = beginPayWindow()
    openSpy.mockClear()
    gotoPay(st, 'https://pay.example.com/x')
    expect(w.location.href).toBe('https://pay.example.com/x')
    expect(openSpy).not.toHaveBeenCalled() // 不再新开窗
  })
  it('预占被拦（handle=null）→ 当前页 location.href 兜底', () => {
    setUA(IOS_UA)
    openSpy.mockReturnValueOnce(null)
    const st = beginPayWindow()
    gotoPay(st, 'https://pay.example.com/x')
    expect(locHref.value).toBe('https://pay.example.com/x')
  })
  it('预占窗口已被用户关掉（closed=true）→ 当前页 location.href 兜底', () => {
    setUA(IOS_UA)
    const w = fakeWin()
    w.closed = true
    openSpy.mockReturnValueOnce(w)
    const st = beginPayWindow()
    gotoPay(st, 'https://pay.example.com/x')
    expect(locHref.value).toBe('https://pay.example.com/x')
  })
  it('取消时关掉空白占位窗', () => {
    setUA(IOS_UA)
    const w = fakeWin()
    openSpy.mockReturnValueOnce(w)
    const st = beginPayWindow()
    cancelPayWindow(st)
    expect(w.close).toHaveBeenCalled()
  })
})
