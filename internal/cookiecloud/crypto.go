// Package cookiecloud 迁移 V2 CookieCloud 的加解密、域名归组与同步编排。
package cookiecloud

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
)

var (
	// ErrCipherTextInvalid 表示密文不是 CryptoJS `Salted__` 格式或长度无效。
	ErrCipherTextInvalid = errors.New("CookieCloud 密文格式无效")
	// ErrDecryptFailed 表示密钥/填充不正确导致解密失败。
	ErrDecryptFailed = errors.New("CookieCloud 解密失败")
)

// CryptKey 复刻 V2 `CookieCloudHelper.__get_crypt_key`
// （app/helper/cookiecloud.py:111–116）：`md5("{key}-{password}")[:16]` 的 UTF-8
// 字节作为口令。
func CryptKey(key, password string) []byte {
	sum := md5.Sum([]byte(key + "-" + password))
	return []byte(hex.EncodeToString(sum[:])[:16])
}

// Decrypt 复刻 V2 `CryptoJsUtils.decrypt`（app/utils/crypto.py:182–208）：
// base64 → `Salted__` + 8 字节盐 → OpenSSL EVP_BytesToKey(48) → AES-256-CBC →
// PKCS#7 去填充。
func Decrypt(encrypted string, passphrase []byte) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encrypted))
	if err != nil || len(raw) < 16 || !bytes.HasPrefix(raw, []byte("Salted__")) {
		return nil, ErrCipherTextInvalid
	}
	salt := raw[8:16]
	keyIV := bytesToKey(passphrase, salt, 48)
	block, err := aes.NewCipher(keyIV[:32])
	if err != nil {
		return nil, ErrDecryptFailed
	}
	ciphertext := raw[16:]
	if len(ciphertext) == 0 || len(ciphertext)%aes.BlockSize != 0 {
		return nil, ErrCipherTextInvalid
	}
	plain := make([]byte, len(ciphertext))
	cipher.NewCBCDecrypter(block, keyIV[32:48]).CryptBlocks(plain, ciphertext)
	return unpadPKCS7(plain, aes.BlockSize)
}

// Encrypt 复刻 V2 `CryptoJsUtils.encrypt`（app/utils/crypto.py:157–179）。
func Encrypt(message, passphrase []byte) (string, error) {
	salt := make([]byte, 8)
	if _, err := rand.Read(salt); err != nil {
		return "", ErrDecryptFailed
	}
	keyIV := bytesToKey(passphrase, salt, 48)
	block, err := aes.NewCipher(keyIV[:32])
	if err != nil {
		return "", ErrDecryptFailed
	}
	padded := padPKCS7(message, aes.BlockSize)
	out := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, keyIV[32:48]).CryptBlocks(out, padded)
	payload := make([]byte, 0, 16+len(out))
	payload = append(payload, []byte("Salted__")...)
	payload = append(payload, salt...)
	payload = append(payload, out...)
	return base64.StdEncoding.EncodeToString(payload), nil
}

// bytesToKey 复刻 V2 `CryptoJsUtils.bytes_to_key`（OpenSSL EVP_BytesToKey）。
func bytesToKey(passphrase, salt []byte, output int) []byte {
	data := make([]byte, 0, len(passphrase)+len(salt))
	data = append(data, passphrase...)
	data = append(data, salt...)
	digest := md5.Sum(data)
	key := append([]byte{}, digest[:]...)
	final := append([]byte{}, key...)
	for len(final) < output {
		material := make([]byte, 0, len(key)+len(data))
		material = append(material, key...)
		material = append(material, data...)
		next := md5.Sum(material)
		key = append([]byte{}, next[:]...)
		final = append(final, key...)
	}
	return final[:output]
}

func unpadPKCS7(data []byte, blockSize int) ([]byte, error) {
	if len(data) == 0 {
		return data, nil
	}
	padding := int(data[len(data)-1])
	if padding <= 0 || padding > blockSize || padding > len(data) {
		return nil, ErrDecryptFailed
	}
	for _, value := range data[len(data)-padding:] {
		if int(value) != padding {
			return nil, ErrDecryptFailed
		}
	}
	return data[:len(data)-padding], nil
}

func padPKCS7(data []byte, blockSize int) []byte {
	padding := blockSize - len(data)%blockSize
	padded := make([]byte, 0, len(data)+padding)
	padded = append(padded, data...)
	for index := 0; index < padding; index++ {
		padded = append(padded, byte(padding))
	}
	return padded
}
