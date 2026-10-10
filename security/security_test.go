package security_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/werhani-houssemeddine/go-archkit/app"
	"github.com/werhani-houssemeddine/go-archkit/errs"
	"github.com/werhani-houssemeddine/go-archkit/security"
	"github.com/werhani-houssemeddine/go-archkit/security/jwtauth"
)

func mustKey(t *testing.T, id string) security.Key {
	k, err := security.ParseKey(id, security.GenerateKey())
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestAESGCM(t *testing.T) {
	k1, k2 := mustKey(t, "k1"), mustKey(t, "k2")
	enc, _ := security.NewAESGCM(k1)
	a, _ := enc.Encrypt([]byte("secret ✓"), nil)
	b, _ := enc.Encrypt([]byte("secret ✓"), nil)
	if a == b {
		t.Fatal("nonce must be random")
	}
	if pt, err := enc.Decrypt(a, nil); err != nil || string(pt) != "secret ✓" {
		t.Fatal(err)
	}
	// tamper
	parts := strings.Split(a, ".")
	last := []byte(parts[3])
	if last[0] == 'A' {
		last[0] = 'B'
	} else {
		last[0] = 'A'
	}
	parts[3] = string(last)
	if _, err := enc.Decrypt(strings.Join(parts, "."), nil); !errors.Is(err, security.ErrDecrypt) {
		t.Fatalf("tampering must be detected: %v", err)
	}
	// associated data
	c, _ := enc.Encrypt([]byte("iban"), []byte("user:1"))
	if _, err := enc.Decrypt(c, []byte("user:2")); err == nil {
		t.Fatal("aad mismatch must fail")
	}
	// rotation
	rotated, _ := security.NewAESGCM(k2, k1)
	if pt, err := rotated.Decrypt(a, nil); err != nil || string(pt) != "secret ✓" || !rotated.NeedsReencryption(a) {
		t.Fatal("old ciphertexts decrypt after rotation")
	}
	if _, err := security.ParseKey("bad", "abcd"); err == nil {
		t.Fatal("short key must be rejected")
	}
}

func TestPBKDF2AndChained(t *testing.T) {
	weak := security.PBKDF2Hasher{Iterations: 1000}
	strong := security.PBKDF2Hasher{Iterations: 2000}
	h, _ := weak.Hash("correct horse")
	if ok, _ := strong.Verify("correct horse", h); !ok {
		t.Fatal("verify with stored params")
	}
	if ok, _ := strong.Verify("wrong", h); ok {
		t.Fatal("wrong password")
	}
	if !strong.NeedsRehash(h) || weak.NeedsRehash(h) {
		t.Fatal("rehash detection")
	}
	peppered := security.PBKDF2Hasher{Iterations: 1000, Pepper: []byte("p1")}
	if ok, _ := (security.PBKDF2Hasher{Iterations: 1000, Pepper: []byte("p2")}).Verify("pw", must(peppered.Hash("pw"))); ok {
		t.Fatal("pepper must matter")
	}
	legacy := legacyHasher{}
	chained := security.ChainedHasher{Primary: strong, Legacy: []app.PasswordHasher{legacy}}
	if ok, _ := chained.Verify("pw", "legacy:pw"); !ok || !chained.NeedsRehash("legacy:pw") {
		t.Fatal("legacy hashes verify and need rehash")
	}
	dummy := must(weak.Hash("dummy"))
	if err := security.VerifyPassword(weak, "pw", "", dummy); !errors.Is(err, errs.ErrInvalidCredentials) {
		t.Fatalf("unknown user: %v", err)
	}
}

type legacyHasher struct{}

func (legacyHasher) Hash(p string) (string, error)    { return "legacy:" + p, nil }
func (legacyHasher) Verify(p, h string) (bool, error) { return h == "legacy:"+p, nil }
func (legacyHasher) NeedsRehash(string) bool          { return true }
func must(s string, err error) string {
	if err != nil {
		panic(err)
	}
	return s
}

func TestRandomHasNoModuloBias(t *testing.T) {
	if s := security.OTP(6); len(s) != 6 || strings.Trim(s, "0123456789") != "" {
		t.Fatal(s)
	}
	if c := security.ReadableCode(8); len(c) != 9 || c[4] != '-' {
		t.Fatal(c)
	}
	counts := map[rune]int{}
	for _, c := range security.RandomString(62_000, security.Alphanumeric) {
		counts[c]++
	}
	lo, hi := 1<<30, 0
	for _, n := range counts {
		lo, hi = min(lo, n), max(hi, n)
	}
	if float64(hi)/float64(lo) > 1.25 {
		t.Fatalf("distribution skewed: %d..%d", lo, hi)
	}
	tok, hash := security.OpaqueToken()
	if security.SHA256Hex(tok) != hash {
		t.Fatal("opaque token hash")
	}
	sig := "sha256=" + security.HMACSHA256Hex([]byte("whsec"), []byte("payload"))
	if !security.VerifySignature([]byte("whsec"), []byte("payload"), sig, "sha256=") || security.VerifySignature([]byte("whsec"), []byte("x"), sig, "sha256=") {
		t.Fatal("webhook signature")
	}
}

func TestJWT(t *testing.T) {
	secret := []byte(strings.Repeat("s", 40))
	svc, err := jwtauth.New(jwtauth.Options{Secret: secret, Issuer: "auth.acme", Audience: []string{"api"}, DefaultTTL: 5 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	tok, err := svc.Issue(ctx, app.Claims{Subject: "u1", Roles: []string{"admin"}, TenantID: "t1"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	c, err := svc.Verify(ctx, "Bearer "+tok)
	if err != nil || c.Subject != "u1" || c.Roles[0] != "admin" || c.TenantID != "t1" || c.TokenID == "" {
		t.Fatalf("verify: %+v %v", c, err)
	}
	none, _ := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.MapClaims{"sub": "u1", "iss": "auth.acme", "aud": "api", "exp": time.Now().Add(time.Hour).Unix()}).SignedString(jwt.UnsafeAllowNoneSignatureType)
	if _, err := svc.Verify(ctx, none); !errors.Is(err, errs.ErrInvalidToken) {
		t.Fatalf("alg=none must be rejected: %v", err)
	}
	expired, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"sub": "u1", "iss": "auth.acme", "aud": "api", "exp": time.Now().Add(-time.Hour).Unix()}).SignedString(secret)
	if _, err := svc.Verify(ctx, expired); !errors.Is(err, errs.ErrExpiredToken) {
		t.Fatalf("expired: %v", err)
	}
	forged, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"sub": "attacker", "iss": "auth.acme", "aud": "api", "exp": time.Now().Add(time.Hour).Unix()}).SignedString([]byte(strings.Repeat("x", 40)))
	if _, err := svc.Refresh(ctx, forged, 0); !errors.Is(err, errs.ErrInvalidToken) {
		t.Fatal("refresh must verify first")
	}
	if _, err := jwtauth.New(jwtauth.Options{Secret: []byte("short")}); err == nil {
		t.Fatal("weak secret")
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	ed, _ := jwtauth.New(jwtauth.Options{Method: jwt.SigningMethodEdDSA, PrivateKey: priv, PublicKey: pub})
	edTok, _ := ed.Issue(ctx, app.Claims{Subject: "svc"}, time.Minute)
	verifyOnly, _ := jwtauth.New(jwtauth.Options{Method: jwt.SigningMethodEdDSA, PublicKey: pub})
	if c, err := verifyOnly.Verify(ctx, edTok); err != nil || c.Subject != "svc" {
		t.Fatalf("eddsa: %v", err)
	}
}
