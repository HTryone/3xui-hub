package threexui

import (
	"fmt"

	"controlhub-server/internal/driver/internal/secrets"
)

// NodeRow 是 nodes 表里驱动需要的列（password / api_key 保持**库内密文**形态）。
//
// 与 backend/app/Models/Node.php 的 casts 对齐：
//   - password / api_key 用 Crypt::encryptString 加密入库；
//   - verify_ssl 默认 false。
type NodeRow struct {
	Scheme      string
	Host        string
	Port        int
	WebBasePath string
	Username    string
	// PasswordCipher / APIKeyCipher 是库内密文字面量（NULL → nil）。
	PasswordCipher *string
	APIKeyCipher   *string
	VerifySSL      bool
}

// NewForNode 由节点行构造客户端，并按 Node::password / Node::api_key 的
// accessor 口径**严格解密**（PHP 侧 Crypt::decryptString 抛
// DecryptException 时，Laravel 会把整个属性读取变成异常 —— 上层据此判定节点不可用）。
//
// key 是 APP_KEY 解出的 32 字节 AES 密钥（crypto.ParseAppKey）。
// 这是「节点密码/api_key 用 crypto 解密」的唯一入口：业务层不要自己解，
// 否则两套口径（严格 vs 宽松）会在不同调用点分叉。
func NewForNode(row NodeRow, key []byte) (*Client, error) {
	password, err := secrets.DecryptStrictPtr(row.PasswordCipher, key)
	if err != nil {
		return nil, fmt.Errorf("解密节点 password 失败: %w", err)
	}
	apiKey, err := secrets.DecryptStrictPtr(row.APIKeyCipher, key)
	if err != nil {
		return nil, fmt.Errorf("解密节点 api_key 失败: %w", err)
	}

	cfg := Config{
		Scheme:      row.Scheme,
		Host:        row.Host,
		Port:        row.Port,
		WebBasePath: row.WebBasePath,
		Username:    row.Username,
		VerifySSL:   row.VerifySSL,
	}
	if password != nil {
		cfg.Password = *password
	}
	if apiKey != nil {
		cfg.APIKey = *apiKey
	}
	return New(cfg), nil
}

// NewDriverForNode 是 NewForNode 的驱动版（业务层按 panel.Driver 使用）。
func NewDriverForNode(row NodeRow, key []byte) (*Driver, error) {
	client, err := NewForNode(row, key)
	if err != nil {
		return nil, err
	}
	return NewDriverWithClient(client), nil
}
