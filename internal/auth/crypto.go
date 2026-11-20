package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
)

// SecretBox 使用环境主密钥加密需要持久化的敏感配置。
type SecretBox struct {
	aead cipher.AEAD
}

// NewSecretBox 从任意长度主密钥派生 AES-256-GCM 密钥。
func NewSecretBox(masterKey string) (*SecretBox, error) {
	if len(masterKey) < 16 {
		return nil, errors.New("主密钥至少需要 16 字节")
	}
	key := sha256.Sum256([]byte(masterKey))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, fmt.Errorf("创建 AES: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("创建 GCM: %w", err)
	}
	return &SecretBox{aead: aead}, nil
}

// Encrypt 加密明文并返回带版本前缀的 URL-safe 字符串。
func (b *SecretBox) Encrypt(plainText string) (string, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("生成加密随机数: %w", err)
	}
	cipherText := b.aead.Seal(nonce, nonce, []byte(plainText), []byte("moviepilot:v1"))
	return "v1:" + base64.RawURLEncoding.EncodeToString(cipherText), nil
}

// Decrypt 解密由 Encrypt 生成的字符串。
func (b *SecretBox) Decrypt(encoded string) (string, error) {
	if len(encoded) < 4 || encoded[:3] != "v1:" {
		return "", errors.New("不支持的密文版本")
	}
	cipherText, err := base64.RawURLEncoding.DecodeString(encoded[3:])
	if err != nil {
		return "", errors.New("密文编码无效")
	}
	if len(cipherText) < b.aead.NonceSize() {
		return "", errors.New("密文长度无效")
	}
	nonce, body := cipherText[:b.aead.NonceSize()], cipherText[b.aead.NonceSize():]
	plainText, err := b.aead.Open(nil, nonce, body, []byte("moviepilot:v1"))
	if err != nil {
		return "", errors.New("密文校验失败（通常是 MOVIEPILOT_MASTER_KEY 变更或未设置，历史密文无法解密；恢复原密钥或重新录入站点凭据）")
	}
	return string(plainText), nil
}
