package notification

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"io"
	"math/big"
	"net/url"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/hkdf"
)

// webPushRecordSize 是 RFC 8291 单条记录大小（rs）。
const webPushRecordSize = 4096

// encryptWebPushPayload 实现 RFC 8291 `aes128gcm` 单记录加密。
func encryptWebPushPayload(payload []byte, p256dhB64, authB64 string) ([]byte, error) {
	userPublicBytes, err := decodeWebPushBase64(p256dhB64)
	if err != nil || len(userPublicBytes) != 65 {
		return nil, errWebPushConfig
	}
	authSecret, err := decodeWebPushBase64(authB64)
	if err != nil || len(authSecret) == 0 {
		return nil, errWebPushConfig
	}
	userPublic, err := ecdh.P256().NewPublicKey(userPublicBytes)
	if err != nil {
		return nil, errWebPushConfig
	}
	serverPrivate, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, errWebPushUnavailable
	}
	sharedSecret, err := serverPrivate.ECDH(userPublic)
	if err != nil {
		return nil, errWebPushConfig
	}
	serverPublic := serverPrivate.PublicKey().Bytes()

	keyInfo := make([]byte, 0, len("WebPush: info\x00")+130)
	keyInfo = append(keyInfo, "WebPush: info\x00"...)
	keyInfo = append(keyInfo, userPublicBytes...)
	keyInfo = append(keyInfo, serverPublic...)
	ikm, err := hkdfFull(sharedSecret, authSecret, keyInfo, 32)
	if err != nil {
		return nil, errWebPushUnavailable
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, errWebPushUnavailable
	}
	prk := hkdf.Extract(sha256.New, ikm, salt)
	cek, err := hkdfReader(hkdf.Expand(sha256.New, prk, []byte("Content-Encoding: aes128gcm\x00")), 16)
	if err != nil {
		return nil, errWebPushUnavailable
	}
	nonce, err := hkdfReader(hkdf.Expand(sha256.New, prk, []byte("Content-Encoding: nonce\x00")), 12)
	if err != nil {
		return nil, errWebPushUnavailable
	}

	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, errWebPushUnavailable
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, errWebPushUnavailable
	}
	record := make([]byte, 0, len(payload)+1)
	record = append(record, payload...)
	record = append(record, 0x02) // 单记录 padding delimiter
	ciphertext := gcm.Seal(nil, nonce, record, nil)

	header := make([]byte, 0, 16+4+1+len(serverPublic))
	header = append(header, salt...)
	length := make([]byte, 4)
	binary.BigEndian.PutUint32(length, uint32(webPushRecordSize))
	header = append(header, length...)
	header = append(header, byte(len(serverPublic)))
	header = append(header, serverPublic...)
	return append(header, ciphertext...), nil
}

// vapidAuthorization 生成 VAPID `Authorization: vapid t=<jwt>, k=<key>` 头。
func vapidAuthorization(
	endpoint, subject, privateKeyB64, publicKeyB64 string,
	now time.Time,
) (string, error) {
	privateKey, err := parseVAPIDPrivateKey(privateKeyB64)
	if err != nil {
		return "", err
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", errWebPushConfig
	}
	subject = strings.TrimSpace(subject)
	if subject == "" {
		return "", errWebPushConfig
	}
	claims := jwt.MapClaims{
		"aud": parsed.Scheme + "://" + parsed.Host,
		"exp": now.Add(12 * time.Hour).Unix(),
		"sub": subject,
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodES256, claims).SignedString(privateKey)
	if err != nil {
		return "", errWebPushConfig
	}
	publicKey := strings.TrimSpace(publicKeyB64)
	if publicKey == "" {
		raw := elliptic.Marshal(elliptic.P256(), privateKey.PublicKey.X, privateKey.PublicKey.Y)
		publicKey = base64.RawURLEncoding.EncodeToString(raw)
	}
	return "vapid t=" + signed + ", k=" + publicKey, nil
}

func parseVAPIDPrivateKey(value string) (*ecdsa.PrivateKey, error) {
	raw, err := decodeWebPushBase64(value)
	if err != nil || len(raw) != 32 {
		return nil, errWebPushConfig
	}
	curve := elliptic.P256()
	x, y := curve.ScalarBaseMult(raw)
	if x == nil || y == nil {
		return nil, errWebPushConfig
	}
	key := &ecdsa.PrivateKey{PublicKey: ecdsa.PublicKey{Curve: curve, X: x, Y: y}, D: new(big.Int).SetBytes(raw)}
	if key.D.Sign() == 0 {
		return nil, errWebPushConfig
	}
	return key, nil
}

func hkdfFull(secret, salt, info []byte, length int) ([]byte, error) {
	return hkdfReader(hkdf.New(sha256.New, secret, salt, info), length)
}

func hkdfReader(reader io.Reader, length int) ([]byte, error) {
	out := make([]byte, length)
	if _, err := io.ReadFull(reader, out); err != nil {
		return nil, err
	}
	return out, nil
}

func decodeWebPushBase64(value string) ([]byte, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, errWebPushConfig
	}
	if decoded, err := base64.RawURLEncoding.DecodeString(value); err == nil {
		return decoded, nil
	}
	if decoded, err := base64.URLEncoding.DecodeString(value); err == nil {
		return decoded, nil
	}
	return base64.StdEncoding.DecodeString(value)
}
