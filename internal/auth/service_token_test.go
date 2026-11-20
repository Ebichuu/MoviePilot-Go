package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// tokUserStore 只实现鉴权需要的最小行为，便于覆盖分支而不引入数据库。
type tokUserStore struct {
	user User
	err  error
}

func (s tokUserStore) UserByName(context.Context, string) (User, error) {
	if s.err != nil {
		return User{}, s.err
	}
	return s.user, nil
}

func (s tokUserStore) UserByID(context.Context, int64) (User, error) {
	if s.err != nil {
		return User{}, s.err
	}
	return s.user, nil
}

func TestHashPasswordRejectsShortPassword(t *testing.T) {
	if _, err := HashPassword("1234567"); err == nil {
		t.Fatal("少于 8 位应当报错")
	}
	if _, err := HashPassword("12345678"); err != nil {
		t.Fatalf("8 位应当通过: %v", err)
	}
}

func TestHashPasswordIsSaltedAndIrreversible(t *testing.T) {
	hash, err := HashPassword("correct-horse")
	if err != nil {
		t.Fatalf("生成摘要失败: %v", err)
	}
	if hash == "correct-horse" {
		t.Fatal("摘要不能等于明文")
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte("correct-horse")); err != nil {
		t.Fatalf("摘要应能验证通过: %v", err)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte("wrong")); err == nil {
		t.Fatal("错误密码不应通过")
	}
	other, err := HashPassword("correct-horse")
	if err != nil {
		t.Fatalf("二次生成摘要失败: %v", err)
	}
	if other == hash {
		t.Fatal("bcrypt 应带随机盐，两次摘要不应相同")
	}
}

func newTestService(t *testing.T, store UserStore, lifetime time.Duration) *Service {
	t.Helper()
	return NewService(store, "jwt-secret-for-test", "resource-secret-for-test", lifetime)
}

func TestAuthenticateSucceedsWithCorrectPassword(t *testing.T) {
	hash, err := HashPassword("password-123")
	if err != nil {
		t.Fatalf("生成摘要失败: %v", err)
	}
	store := tokUserStore{user: User{ID: 7, Username: "admin", Password: hash, SuperUser: true}}
	service := newTestService(t, store, time.Hour)

	user, err := service.Authenticate(context.Background(), "  admin  ", "password-123")
	if err != nil {
		t.Fatalf("认证失败: %v", err)
	}
	if user.ID != 7 || user.Username != "admin" {
		t.Fatalf("返回的用户不对: %+v", user)
	}
}

func TestAuthenticateDoesNotRevealWhetherUserExists(t *testing.T) {
	hash, err := HashPassword("password-123")
	if err != nil {
		t.Fatalf("生成摘要失败: %v", err)
	}
	knownStore := tokUserStore{user: User{Username: "admin", Password: hash}}
	unknownStore := tokUserStore{err: errors.New("用户不存在")}

	_, knownErr := newTestService(t, knownStore, time.Hour).Authenticate(context.Background(), "admin", "wrong-password")
	_, unknownErr := newTestService(t, unknownStore, time.Hour).Authenticate(context.Background(), "ghost", "wrong-password")
	if knownErr == nil || unknownErr == nil {
		t.Fatal("两种失败都应返回错误")
	}
	if knownErr.Error() != unknownErr.Error() {
		t.Fatalf("错误文案不应区分「用户不存在」与「密码错误」: %q vs %q", knownErr, unknownErr)
	}
}

func TestAuthenticateRejectsWrongPassword(t *testing.T) {
	hash, err := HashPassword("password-123")
	if err != nil {
		t.Fatalf("生成摘要失败: %v", err)
	}
	store := tokUserStore{user: User{Username: "admin", Password: hash}}
	if _, err := newTestService(t, store, time.Hour).Authenticate(context.Background(), "admin", "password-124"); err == nil {
		t.Fatal("错误密码应当失败")
	}
}

func TestAccessTokenRoundTrip(t *testing.T) {
	service := newTestService(t, tokUserStore{}, time.Hour)
	token, err := service.AccessToken(User{ID: 1, Username: "admin", SuperUser: true})
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	claims, err := service.VerifyAccessToken(token)
	if err != nil {
		t.Fatalf("校验失败: %v", err)
	}
	if claims.Username != "admin" || !claims.SuperUser {
		t.Fatalf("载荷不对: %+v", claims)
	}
	if claims.Purpose != "authentication" {
		t.Fatalf("purpose = %q", claims.Purpose)
	}
}

func TestResourceTokenIsNotAcceptedAsAccessToken(t *testing.T) {
	service := newTestService(t, tokUserStore{}, time.Hour)
	resourceToken, err := service.ResourceToken(User{ID: 1, Username: "admin"}, time.Hour)
	if err != nil {
		t.Fatalf("签发资源令牌失败: %v", err)
	}
	if _, err := service.VerifyAccessToken(resourceToken); err == nil {
		t.Fatal("资源令牌不能当访问令牌用（用途隔离）")
	}

	accessToken, err := service.AccessToken(User{ID: 1, Username: "admin"})
	if err != nil {
		t.Fatalf("签发访问令牌失败: %v", err)
	}
	if _, err := service.VerifyResourceToken(accessToken); err == nil {
		t.Fatal("访问令牌不能当资源令牌用（用途隔离）")
	}
}

func TestExpiredAccessTokenIsRejected(t *testing.T) {
	service := newTestService(t, tokUserStore{}, -time.Minute)
	token, err := service.AccessToken(User{ID: 1, Username: "admin"})
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	if _, err := service.VerifyAccessToken(token); err == nil {
		t.Fatal("过期令牌应当被拒绝")
	}
}

func TestVerifyRejectsForeignSecret(t *testing.T) {
	issuer := newTestService(t, tokUserStore{}, time.Hour)
	token, err := issuer.AccessToken(User{ID: 1, Username: "admin"})
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	other := NewService(tokUserStore{}, "another-secret", "another-resource", time.Hour)
	if _, err := other.VerifyAccessToken(token); err == nil {
		t.Fatal("换密钥后不应验签通过")
	}
}

func TestVerifyRejectsGarbage(t *testing.T) {
	service := newTestService(t, tokUserStore{}, time.Hour)
	if _, err := service.VerifyAccessToken("not-a-jwt"); err == nil {
		t.Fatal("非法令牌应当报错")
	}
}
