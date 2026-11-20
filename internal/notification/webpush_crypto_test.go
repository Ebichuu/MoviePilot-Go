package notification

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/hkdf"
)

// rwReceiver 扮演浏览器侧：持有订阅私钥与 auth secret，把服务端发来的
// aes128gcm 单记录消息解回明文。能和它互通，才说明格式真的符合 RFC 8291。
type rwReceiver struct {
	private    *ecdh.PrivateKey
	publicB64  string
	authSecret []byte
	authB64    string
}

func newRWReceiver(t *testing.T) rwReceiver {
	t.Helper()
	private, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("生成订阅密钥失败: %v", err)
	}
	secret := make([]byte, 16)
	if _, err := rand.Read(secret); err != nil {
		t.Fatalf("生成 auth secret 失败: %v", err)
	}
	return rwReceiver{
		private:    private,
		publicB64:  base64.RawURLEncoding.EncodeToString(private.PublicKey().Bytes()),
		authSecret: secret,
		authB64:    base64.RawURLEncoding.EncodeToString(secret),
	}
}

// open 只做解密，返回原始记录（含尾部 0x02 与可能的填充）。
func (r rwReceiver) open(message []byte) ([]byte, error) {
	if len(message) < 22 {
		return nil, errWebPushConfig
	}
	idLength := int(message[20])
	if idLength != 65 || len(message) < 21+idLength+16 {
		return nil, errWebPushConfig
	}
	salt := message[0:16]
	serverPublicBytes := message[21 : 21+idLength]
	ciphertext := message[21+idLength:]

	serverPublic, err := ecdh.P256().NewPublicKey(serverPublicBytes)
	if err != nil {
		return nil, err
	}
	shared, err := r.private.ECDH(serverPublic)
	if err != nil {
		return nil, err
	}
	keyInfo := append([]byte("WebPush: info\x00"), r.private.PublicKey().Bytes()...)
	keyInfo = append(keyInfo, serverPublicBytes...)
	ikm, err := hkdfFull(shared, r.authSecret, keyInfo, 32)
	if err != nil {
		return nil, err
	}
	prk := hkdf.Extract(sha256.New, ikm, salt)
	cek, err := hkdfReader(hkdf.Expand(sha256.New, prk, []byte("Content-Encoding: aes128gcm\x00")), 16)
	if err != nil {
		return nil, err
	}
	nonce, err := hkdfReader(hkdf.Expand(sha256.New, prk, []byte("Content-Encoding: nonce\x00")), 12)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, nonce, ciphertext, nil)
}

func (r rwReceiver) decrypt(t *testing.T, message []byte) []byte {
	t.Helper()
	if len(message) >= 20 {
		if size := binary.BigEndian.Uint32(message[16:20]); size != uint32(webPushRecordSize) {
			t.Fatalf("rs = %d，期望 %d", size, webPushRecordSize)
		}
	}
	plaintext, err := r.open(message)
	if err != nil {
		t.Fatalf("解密失败（说明实现与 RFC 8291 不一致）: %v", err)
	}
	trimmed := bytes.TrimRight(plaintext, "\x00")
	if len(trimmed) < 1 || trimmed[len(trimmed)-1] != 0x02 {
		t.Fatalf("缺少单记录分隔符 0x02: %x", plaintext)
	}
	return trimmed[:len(trimmed)-1]
}

func TestEncryptWebPushPayloadRoundTrip(t *testing.T) {
	receiver := newRWReceiver(t)
	payload := []byte(`{"title":"下载完成","body":"示例剧 S01E01","url":"/detail/1"}`)

	message, err := encryptWebPushPayload(payload, receiver.publicB64, receiver.authB64)
	if err != nil {
		t.Fatalf("加密失败: %v", err)
	}
	if got := receiver.decrypt(t, message); !bytes.Equal(got, payload) {
		t.Fatalf("解密结果 = %q，期望 %q", got, payload)
	}

	// 同一明文两次加密必须不同：salt 与临时密钥都是随机的。
	again, err := encryptWebPushPayload(payload, receiver.publicB64, receiver.authB64)
	if err != nil {
		t.Fatalf("二次加密失败: %v", err)
	}
	if bytes.Equal(message, again) {
		t.Fatal("两次加密结果相同，说明 salt/临时密钥没有随机化")
	}
	if got := receiver.decrypt(t, again); !bytes.Equal(got, payload) {
		t.Fatalf("二次解密结果 = %q", got)
	}
}

func TestEncryptWebPushPayloadRejectsTampering(t *testing.T) {
	receiver := newRWReceiver(t)
	message, err := encryptWebPushPayload([]byte("hello"), receiver.publicB64, receiver.authB64)
	if err != nil {
		t.Fatalf("加密失败: %v", err)
	}
	tampered := append([]byte(nil), message...)
	tampered[len(tampered)-1] ^= 0xFF
	if _, err := receiver.open(tampered); err == nil {
		t.Fatal("被篡改的密文必须校验失败")
	}
	// 换一个订阅者的私钥也解不开：说明用的是订阅者公钥，而不是固定密钥。
	other := newRWReceiver(t)
	if _, err := other.open(message); err == nil {
		t.Fatal("无关私钥不应能解密")
	}
}

func TestEncryptWebPushPayloadValidatesSubscription(t *testing.T) {
	receiver := newRWReceiver(t)
	shortKey := base64.RawURLEncoding.EncodeToString(make([]byte, 64))
	notOnCurve := base64.RawURLEncoding.EncodeToString(append([]byte{0x04}, make([]byte, 64)...))

	cases := map[string]struct{ p256dh, auth string }{
		"公钥为空":        {"", receiver.authB64},
		"公钥长度不足":      {shortKey, receiver.authB64},
		"公钥不在曲线上":     {notOnCurve, receiver.authB64},
		"auth 为空":     {receiver.publicB64, ""},
		"公钥不是 base64": {receiver.publicB64[:20] + "!!!!", receiver.authB64},
	}
	for name, testCase := range cases {
		if _, err := encryptWebPushPayload([]byte("x"), testCase.p256dh, testCase.auth); err == nil {
			t.Fatalf("%s 应返回配置错误", name)
		}
	}
}

func TestVAPIDAuthorization(t *testing.T) {
	privateBytes := make([]byte, 32)
	if _, err := rand.Read(privateBytes); err != nil {
		t.Fatalf("生成私钥失败: %v", err)
	}
	privateB64 := base64.RawURLEncoding.EncodeToString(privateBytes)
	privateKey, err := parseVAPIDPrivateKey(privateB64)
	if err != nil {
		t.Fatalf("解析私钥失败: %v", err)
	}
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

	header, err := vapidAuthorization("https://push.example.net/wpush/v2/abc", "mailto:admin@example.com", privateB64, "", now)
	if err != nil {
		t.Fatalf("生成 VAPID 头失败: %v", err)
	}
	if !strings.HasPrefix(header, "vapid t=") {
		t.Fatalf("头格式不对: %q", header)
	}
	parts := strings.SplitN(strings.TrimPrefix(header, "vapid t="), ", k=", 2)
	if len(parts) != 2 {
		t.Fatalf("缺少 k= 段: %q", header)
	}
	token, publicKey := parts[0], parts[1]

	// k 必须是同一把 P-256 公钥的未压缩点（否则推送服务无法验签）。
	rawPublic, err := decodeWebPushBase64(publicKey)
	if err != nil {
		t.Fatalf("公钥解码失败: %v", err)
	}
	expectedPublic := elliptic.Marshal(elliptic.P256(), privateKey.PublicKey.X, privateKey.PublicKey.Y)
	if !bytes.Equal(rawPublic, expectedPublic) {
		t.Fatalf("k 段与私钥不匹配: %x != %x", rawPublic, expectedPublic)
	}

	// 时间基准钉在签发时刻：jwt 默认用真实时间校验 exp，
	// 这个固定日期一过，测试就会变成「token 已过期」的假失败。
	parsed, err := jwt.Parse(token, func(*jwt.Token) (any, error) {
		return &privateKey.PublicKey, nil
	}, jwt.WithValidMethods([]string{"ES256"}), jwt.WithTimeFunc(func() time.Time { return now }))
	if err != nil {
		t.Fatalf("JWT 校验失败: %v", err)
	}
	claims, ok := parsed.Claims.(jwt.MapClaims)
	if !ok {
		t.Fatalf("claims 类型异常: %T", parsed.Claims)
	}
	if aud := claims["aud"]; aud != "https://push.example.net" {
		t.Fatalf("aud = %#v，期望只含 scheme+host", aud)
	}
	if sub := claims["sub"]; sub != "mailto:admin@example.com" {
		t.Fatalf("sub = %#v", sub)
	}
	if exp, ok := claims["exp"].(float64); !ok || int64(exp) != now.Add(12*time.Hour).Unix() {
		t.Fatalf("exp = %#v，期望 12 小时后", claims["exp"])
	}

	// 显式传入的公钥应原样（去空格）出现在头里。
	explicit, err := vapidAuthorization("https://push.example/x", "mailto:a@b.c", privateB64, "  given-key  ", now)
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	if !strings.HasSuffix(explicit, ", k=given-key") {
		t.Fatalf("显式公钥未被采用: %q", explicit)
	}
}

func TestVAPIDAuthorizationValidatesInputs(t *testing.T) {
	privateBytes := make([]byte, 32)
	if _, err := rand.Read(privateBytes); err != nil {
		t.Fatalf("生成私钥失败: %v", err)
	}
	privateB64 := base64.RawURLEncoding.EncodeToString(privateBytes)
	now := time.Now()

	cases := map[string]struct{ endpoint, subject, private string }{
		"端点含空格":       {"not a url", "mailto:a@b.c", privateB64},
		"端点缺 scheme":  {"//push.example/x", "mailto:a@b.c", privateB64},
		"subject 为空":  {"https://push.example/x", "   ", privateB64},
		"私钥长度不对":      {"https://push.example/x", "mailto:a@b.c", base64.RawURLEncoding.EncodeToString(make([]byte, 31))},
		"私钥不是 base64": {"https://push.example/x", "mailto:a@b.c", "!!!!"},
		"全零私钥":        {"https://push.example/x", "mailto:a@b.c", base64.RawURLEncoding.EncodeToString(make([]byte, 32))},
	}
	for name, testCase := range cases {
		if _, err := vapidAuthorization(testCase.endpoint, testCase.subject, testCase.private, "", now); err == nil {
			t.Fatalf("%s 应返回配置错误", name)
		}
	}
}

func TestDecodeWebPushBase64(t *testing.T) {
	raw := []byte{1, 2, 3, 4}
	cases := map[string]string{
		"raw url":    base64.RawURLEncoding.EncodeToString(raw),
		"padded url": base64.URLEncoding.EncodeToString(raw),
		"标准 base64":  base64.StdEncoding.EncodeToString(raw),
	}
	for name, encoded := range cases {
		decoded, err := decodeWebPushBase64(encoded)
		if err != nil {
			t.Fatalf("%s 解码失败: %v", name, err)
		}
		if !bytes.Equal(decoded, raw) {
			t.Fatalf("%s 解码结果 = %x", name, decoded)
		}
	}
	if decoded, err := decodeWebPushBase64("  " + cases["raw url"] + "  "); err != nil || !bytes.Equal(decoded, raw) {
		t.Fatalf("首尾空白应被忽略: %v %x", err, decoded)
	}
	if _, err := decodeWebPushBase64(""); err == nil {
		t.Fatal("空串应报错")
	}
	if _, err := decodeWebPushBase64("!!!!"); err == nil {
		t.Fatal("非法 base64 应报错")
	}
}

func TestParseVAPIDPrivateKey(t *testing.T) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("生成私钥失败: %v", err)
	}
	key, err := parseVAPIDPrivateKey(base64.RawURLEncoding.EncodeToString(raw))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if key.D.Sign() <= 0 {
		t.Fatal("D 必须为正")
	}
	if key.Curve.Params().Name != "P-256" {
		t.Fatalf("曲线 = %s", key.Curve.Params().Name)
	}
	// 标量必须与私钥一致：否则签名用的其实不是这把钥匙。
	if key.D.Cmp(new(big.Int).SetBytes(raw)) != 0 {
		t.Fatal("D 与输入标量不一致")
	}
	if _, err := parseVAPIDPrivateKey(base64.RawURLEncoding.EncodeToString(raw[:31])); err == nil {
		t.Fatal("31 字节私钥应被拒绝")
	}
}

func TestHKDFHelpers(t *testing.T) {
	secret := []byte("secret")
	salt := []byte("salt")
	info := []byte("info")

	first, err := hkdfFull(secret, salt, info, 32)
	if err != nil {
		t.Fatalf("派生失败: %v", err)
	}
	if len(first) != 32 {
		t.Fatalf("长度 = %d", len(first))
	}
	second, err := hkdfFull(secret, salt, info, 32)
	if err != nil {
		t.Fatalf("派生失败: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("相同输入必须得到相同输出")
	}
	other, err := hkdfFull(secret, []byte("other"), info, 32)
	if err != nil {
		t.Fatalf("派生失败: %v", err)
	}
	if bytes.Equal(first, other) {
		t.Fatal("不同 salt 不应得到相同输出")
	}
	if empty, err := hkdfReader(hkdf.New(sha256.New, secret, salt, info), 0); err != nil || len(empty) != 0 {
		t.Fatalf("长度 0 应返回空切片: %v %d", err, len(empty))
	}
	if _, err := hkdfReader(hkdf.New(sha256.New, secret, salt, info), 255*32+1); err == nil {
		t.Fatal("超过 HKDF 输出上限应报错")
	}
}
