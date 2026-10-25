// Package tgbot 是 Telegram Bot API 的极简客户端：发文本、发文件、验证 token。
//
// 只依赖标准库（net/http + mime/multipart），与面板其余 HTTP 客户端一致。
// token 由调用方从配置解密后传入，本包不落任何持久化。
package tgbot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// apiBase 是官方 Bot API 根地址。
const apiBase = "https://api.telegram.org"

// Client 是一个 token 对应的 Bot 客户端。
type Client struct {
	token string
	httpc *http.Client
}

// NewClient 构造客户端。token 由调用方解密后传入；httpc nil 用 30s 超时的默认 client。
func NewClient(token string, httpc *http.Client) *Client {
	if httpc == nil {
		httpc = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{token: token, httpc: httpc}
}

// BotInfo 是 getMe 的返回。
type BotInfo struct {
	Username string `json:"username"`
	FirstName string `json:"first_name"`
}

// GetMe 验证 token 并返回 bot 用户名（token 无效时返回错误）。
func (c *Client) GetMe(ctx context.Context) (*BotInfo, error) {
	var out struct {
		OK     bool    `json:"ok"`
		Result BotInfo `json:"result"`
	}
	if err := c.do(ctx, "getMe", nil, "", &out); err != nil {
		return nil, err
	}
	if !out.OK {
		return nil, fmt.Errorf("getMe 失败")
	}
	return &out.Result, nil
}

// SendMessage 发纯文本消息（chatID 为接收方，个人/群/频道均可）。
func (c *Client) SendMessage(ctx context.Context, chatID int64, text string) error {
	payload, err := json.Marshal(map[string]any{
		"chat_id": chatID,
		"text":    text,
	})
	if err != nil {
		return err
	}
	var out struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if err := c.do(ctx, "sendMessage", payload, "application/json", &out); err != nil {
		return err
	}
	if !out.OK {
		return fmt.Errorf("sendMessage 失败: %s", out.Description)
	}
	return nil
}

// SendDocument 发一个文件（multipart/form-data 上传）。caption 可为空。
func (c *Client) SendDocument(ctx context.Context, chatID int64, caption, filePath string) error {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	_ = w.WriteField("chat_id", fmt.Sprintf("%d", chatID))
	if caption != "" {
		_ = w.WriteField("caption", caption)
	}
	fw, err := w.CreateFormFile("document", filepath.Base(filePath))
	if err != nil {
		return err
	}
	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("打开待发文件失败: %w", err)
	}
	defer f.Close()
	if _, err := io.Copy(fw, f); err != nil {
		return fmt.Errorf("写入待发文件失败: %w", err)
	}
	if err := w.Close(); err != nil {
		return err
	}

	var out struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if err := c.do(ctx, "sendDocument", buf.Bytes(), w.FormDataContentType(), &out); err != nil {
		return err
	}
	if !out.OK {
		return fmt.Errorf("sendDocument 失败: %s", out.Description)
	}
	return nil
}

// do 发一次 API 请求并解析 JSON 响应。
func (c *Client) do(ctx context.Context, method string, body []byte, contentType string, out any) error {
	url := fmt.Sprintf("%s/bot%s/%s", apiBase, c.token, method)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.httpc.Do(req)
	if err != nil {
		return fmt.Errorf("请求 Telegram API 失败: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("读取 Telegram 响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Telegram API HTTP %d: %s", resp.StatusCode, truncate(string(data), 300))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
