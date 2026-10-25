package api

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math/rand"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
)

// 图形验证码（GET /api/captcha）。
//
// 与 PHP 版的差异（都只影响观感，不影响接口契约）：
//   - PHP 用 GD 的 imagestring 内置字体；Go 侧没有 GD，这里用内置 5x7 点阵字体放大绘制；
//   - 画面尺寸 120x40、浅灰背景、5 条干扰线、50 个噪声点、4 位字符、点击刷新 —— 与 PHP 一致。
//
// 字符集与 PHP 完全相同：'23456789ABCDEFGHJKLMNPQRSTUVWXYZ'（去掉易混的 0/O/1/I）。
const captchaCharset = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ"

// captchaPNG 生成验证码图片，返回 PNG 字节与明文。
//
// 随机源用 math/rand（视觉噪声不值得消耗 crypto/rand；验证码本身的抗暴力靠服务端限流）。
// 但字符的生成仍用 crypto/rand —— 与 PHP 的 random_int 同级别不可预测。
func captchaPNG(seed *rand.Rand) ([]byte, string) {
	const (
		width  = 120
		height = 40
		scale  = 2
	)

	img := image.NewRGBA(image.Rect(0, 0, width, height))
	bg := color.RGBA{245, 245, 245, 255}
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, bg)
		}
	}

	// 干扰线 5 条：颜色分量 100..200。
	for i := 0; i < 5; i++ {
		c := color.RGBA{
			R: uint8(100 + seed.Intn(101)),
			G: uint8(100 + seed.Intn(101)),
			B: uint8(100 + seed.Intn(101)),
			A: 255,
		}
		drawLine(img,
			seed.Intn(width), seed.Intn(height),
			seed.Intn(width), seed.Intn(height), c)
	}

	// 干扰点 50 个。
	for i := 0; i < 50; i++ {
		c := color.RGBA{
			R: uint8(100 + seed.Intn(101)),
			G: uint8(100 + seed.Intn(101)),
			B: uint8(100 + seed.Intn(101)),
			A: 255,
		}
		img.Set(seed.Intn(width), seed.Intn(height), c)
	}

	// 字符 4 个：每个字符颜色分量 0..100，位置与 PHP 一致（x = 10 + i*28）。
	code := randomCaptchaCode(4)
	for i, ch := range code {
		c := color.RGBA{
			R: uint8(seed.Intn(101)),
			G: uint8(seed.Intn(101)),
			B: uint8(seed.Intn(101)),
			A: 255,
		}
		glyph, ok := font5x7[byte(ch)]
		if !ok {
			continue
		}
		x := 10 + i*28
		y := 12 + seed.Intn(8) // PHP: y 在 25..35 处 imagestring 的基线，这里折算到顶部
		drawGlyph(img, glyph, x, y, scale, c)
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, ""
	}
	return buf.Bytes(), code
}

// randomCaptchaCode 用密码学随机源生成 n 位验证码（与 PHP random_int 同级不可预测）。
func randomCaptchaCode(n int) string {
	rb := randomBytes(n)
	out := make([]byte, n)
	for i := 0; i < n; i++ {
		out[i] = captchaCharset[int(rb[i])%len(captchaCharset)]
	}
	return string(out)
}

// drawLine 是 Bresenham 直线（与 GD 的 imageline 同样不抗锯齿）。
func drawLine(img *image.RGBA, x0, y0, x1, y1 int, c color.RGBA) {
	dx := absInt(x1 - x0)
	dy := -absInt(y1 - y0)
	sx, sy := 1, 1
	if x0 > x1 {
		sx = -1
	}
	if y0 > y1 {
		sy = -1
	}
	err := dx + dy
	for {
		img.Set(x0, y0, c)
		if x0 == x1 && y0 == y1 {
			return
		}
		e2 := 2 * err
		if e2 >= dy {
			err += dy
			x0 += sx
		}
		if e2 <= dx {
			err += dx
			y0 += sy
		}
	}
}

// drawGlyph 按 scale 倍绘制 5x7 点阵字形。
func drawGlyph(img *image.RGBA, glyph [7]byte, x0, y0, scale int, c color.RGBA) {
	for row := 0; row < 7; row++ {
		bits := glyph[row]
		for col := 0; col < 5; col++ {
			if bits&(1<<uint(4-col)) == 0 {
				continue
			}
			for dy := 0; dy < scale; dy++ {
				for dx := 0; dx < scale; dx++ {
					img.Set(x0+col*scale+dx, y0+row*scale+dy, c)
				}
			}
		}
	}
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// handleCaptcha 实现 GET /api/captcha：输出 PNG 并下发带签名、带过期时间的验证码 cookie。
func (h *Handler) handleCaptcha(c *gin.Context) {
	seed := rand.New(rand.NewSource(time.Now().UnixNano() ^ int64(seedFromCtx(c))))
	body, code := captchaPNG(seed)
	if code == "" {
		failStatus(c, "验证码生成失败", 500, http.StatusInternalServerError)
		return
	}

	exp := time.Now().Add(captchaTTLSeconds * time.Second).Unix()
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     captchaCookieName,
		Value:    signCaptcha(h.captchaSecret, code, exp),
		Path:     "/",
		MaxAge:   captchaTTLSeconds,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})

	// 与 PHP 一致：image/png + 禁止缓存。
	writeRaw(c, "image/png", body, map[string]string{
		"Cache-Control": "no-cache, no-store, private",
	})
}

// seedFromCtx 让同一纳秒内的并发请求不会拿到同一个随机种子。
func seedFromCtx(c *gin.Context) uint64 {
	var b [8]byte
	copy(b[:], c.GetHeader("User-Agent"))
	h := uint64(1469598103934665603)
	for _, x := range b {
		h ^= uint64(x)
		h *= 1099511628211
	}
	return h ^ uint64(len(strconv.Itoa(len(c.Request.URL.Path))))
}

// font5x7 是验证码字符集的点阵字形（每行 5 位，高位在左）。
// 只收录 captchaCharset 用到的字符，未收录的字符在绘制时跳过（理论上不会发生）。
var font5x7 = map[byte][7]byte{
	'2': {0x1E, 0x11, 0x01, 0x02, 0x04, 0x08, 0x1F},
	'3': {0x1E, 0x11, 0x01, 0x0E, 0x01, 0x11, 0x0E},
	'4': {0x02, 0x06, 0x0A, 0x12, 0x1F, 0x02, 0x02},
	'5': {0x1F, 0x10, 0x1E, 0x01, 0x01, 0x11, 0x0E},
	'6': {0x06, 0x08, 0x10, 0x1E, 0x11, 0x11, 0x0E},
	'7': {0x1F, 0x01, 0x02, 0x04, 0x08, 0x08, 0x08},
	'8': {0x0E, 0x11, 0x11, 0x0E, 0x11, 0x11, 0x0E},
	'9': {0x0E, 0x11, 0x11, 0x0F, 0x01, 0x02, 0x0C},
	'A': {0x0E, 0x11, 0x11, 0x1F, 0x11, 0x11, 0x11},
	'B': {0x1E, 0x11, 0x11, 0x1E, 0x11, 0x11, 0x1E},
	'C': {0x0E, 0x11, 0x10, 0x10, 0x10, 0x11, 0x0E},
	'D': {0x1C, 0x12, 0x11, 0x11, 0x11, 0x12, 0x1C},
	'E': {0x1F, 0x10, 0x10, 0x1E, 0x10, 0x10, 0x1F},
	'F': {0x1F, 0x10, 0x10, 0x1E, 0x10, 0x10, 0x10},
	'G': {0x0E, 0x11, 0x10, 0x17, 0x11, 0x11, 0x0F},
	'H': {0x11, 0x11, 0x11, 0x1F, 0x11, 0x11, 0x11},
	'J': {0x07, 0x02, 0x02, 0x02, 0x02, 0x12, 0x0C},
	'K': {0x11, 0x12, 0x14, 0x18, 0x14, 0x12, 0x11},
	'L': {0x10, 0x10, 0x10, 0x10, 0x10, 0x10, 0x1F},
	'M': {0x11, 0x1B, 0x15, 0x15, 0x11, 0x11, 0x11},
	'N': {0x11, 0x19, 0x15, 0x13, 0x11, 0x11, 0x11},
	'P': {0x1E, 0x11, 0x11, 0x1E, 0x10, 0x10, 0x10},
	'Q': {0x0E, 0x11, 0x11, 0x11, 0x15, 0x12, 0x0D},
	'R': {0x1E, 0x11, 0x11, 0x1E, 0x14, 0x12, 0x11},
	'S': {0x0F, 0x10, 0x10, 0x0E, 0x01, 0x01, 0x1E},
	'T': {0x1F, 0x04, 0x04, 0x04, 0x04, 0x04, 0x04},
	'U': {0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x0E},
	'V': {0x11, 0x11, 0x11, 0x11, 0x11, 0x0A, 0x04},
	'W': {0x11, 0x11, 0x11, 0x15, 0x15, 0x1B, 0x11},
	'X': {0x11, 0x11, 0x0A, 0x04, 0x0A, 0x11, 0x11},
	'Y': {0x11, 0x11, 0x0A, 0x04, 0x04, 0x04, 0x04},
	'Z': {0x1F, 0x01, 0x02, 0x04, 0x08, 0x10, 0x1F},
}
