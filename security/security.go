// Package security provides cryptography built only on the Go standard library:
// authenticated encryption with key rotation, password hashing, secure random values,
// HMAC signatures and opaque tokens.
package security

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/werhani-houssemeddine/go-archkit/app"
	"github.com/werhani-houssemeddine/go-archkit/errs"
)

/* ================================================================== */
/* AES-256-GCM with key rotation                                       */
/* ================================================================== */

// Key is an encryption key with a short id stored in each ciphertext.
type Key struct {
	ID  string
	Key []byte // exactly 32 bytes
}

// ParseKey decodes a 32-byte key given as hex (64 chars) or base64.
func ParseKey(id, encoded string) (Key, error) {
	var b []byte
	var err error
	if len(encoded) == 64 {
		b, err = hex.DecodeString(encoded)
	} else {
		b, err = base64.StdEncoding.DecodeString(encoded)
	}
	if err != nil || len(b) != 32 {
		return Key{}, fmt.Errorf("security: key %q must be 32 bytes (hex or base64)", id)
	}
	return Key{ID: id, Key: b}, nil
}

// GenerateKey returns a random 32-byte key, hex encoded.
func GenerateKey() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

var keyIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

// AESGCM implements app.Encryptor with AES-256-GCM: confidentiality and tamper
// detection. Format: "v1.<keyID>.<nonce>.<ciphertext+tag>" (base64url). The first key
// encrypts; all keys decrypt, enabling zero-downtime rotation. Optional associated data
// binds a ciphertext to its context ("user:42") so it can't be copied to another row.
type AESGCM struct {
	primary string
	aeads   map[string]cipher.AEAD
}

var _ app.Encryptor = (*AESGCM)(nil)

// NewAESGCM builds the encryptor; keys[0] is the active key.
func NewAESGCM(keys ...Key) (*AESGCM, error) {
	if len(keys) == 0 {
		return nil, errors.New("security: at least one key is required")
	}
	e := &AESGCM{primary: keys[0].ID, aeads: map[string]cipher.AEAD{}}
	for _, k := range keys {
		if !keyIDRe.MatchString(k.ID) {
			return nil, fmt.Errorf("security: invalid key id %q", k.ID)
		}
		if len(k.Key) != 32 {
			return nil, fmt.Errorf("security: key %q must be 32 bytes", k.ID)
		}
		block, err := aes.NewCipher(k.Key)
		if err != nil {
			return nil, err
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			return nil, err
		}
		e.aeads[k.ID] = aead
	}
	return e, nil
}

// Encrypt encrypts plaintext with the active key.
func (e *AESGCM) Encrypt(plaintext, aad []byte) (string, error) {
	aead := e.aeads[e.primary]
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	ct := aead.Seal(nil, nonce, plaintext, aad)
	enc := base64.RawURLEncoding
	return "v1." + e.primary + "." + enc.EncodeToString(nonce) + "." + enc.EncodeToString(ct), nil
}

// ErrDecrypt is returned for malformed or tampered ciphertexts and wrong associated data.
var ErrDecrypt = errs.Define(errs.KindValidation, "DECRYPTION_FAILED", "ciphertext could not be authenticated")

// Decrypt verifies and decrypts.
func (e *AESGCM) Decrypt(payload string, aad []byte) ([]byte, error) {
	parts := strings.Split(payload, ".")
	if len(parts) != 4 || parts[0] != "v1" {
		return nil, ErrDecrypt.New()
	}
	aead, ok := e.aeads[parts[1]]
	if !ok {
		return nil, errs.Internalf("security: unknown encryption key id %q", parts[1])
	}
	nonce, err1 := base64.RawURLEncoding.DecodeString(parts[2])
	ct, err2 := base64.RawURLEncoding.DecodeString(parts[3])
	if err1 != nil || err2 != nil || len(nonce) != aead.NonceSize() {
		return nil, ErrDecrypt.New()
	}
	pt, err := aead.Open(nil, nonce, ct, aad)
	if err != nil {
		return nil, ErrDecrypt.Wrap(err)
	}
	return pt, nil
}

// NeedsReencryption reports ciphertexts produced with a non-active key.
func (e *AESGCM) NeedsReencryption(payload string) bool {
	parts := strings.SplitN(payload, ".", 3)
	return len(parts) < 2 || parts[1] != e.primary
}

/* ================================================================== */
/* Password hashing: PBKDF2-HMAC-SHA256 (stdlib, FIPS-friendly)        */
/* ================================================================== */

// PBKDF2Hasher implements app.PasswordHasher with PBKDF2-HMAC-SHA256 from the standard
// library (no cgo, FIPS 140-3 approved). Format: "pbkdf2-sha256$<iter>$<salt>$<hash>".
// Parameters are stored in the hash, so raising Iterations later is safe and
// NeedsRehash tells you which users to upgrade at login.
type PBKDF2Hasher struct {
	Iterations int    // default 600,000 (OWASP recommendation)
	SaltLen    int    // default 16
	KeyLen     int    // default 32
	Pepper     []byte // optional server-side secret mixed in via HMAC (keep it out of the DB)
}

var _ app.PasswordHasher = PBKDF2Hasher{}

func (h PBKDF2Hasher) params() (int, int, int) {
	it, sl, kl := h.Iterations, h.SaltLen, h.KeyLen
	if it <= 0 {
		it = 600_000
	}
	if sl <= 0 {
		sl = 16
	}
	if kl <= 0 {
		kl = 32
	}
	return it, sl, kl
}

func (h PBKDF2Hasher) input(plain string) string {
	if len(h.Pepper) == 0 {
		return plain
	}
	m := hmac.New(sha256.New, h.Pepper)
	m.Write([]byte(plain))
	return base64.StdEncoding.EncodeToString(m.Sum(nil))
}

// Hash returns a new salted hash.
func (h PBKDF2Hasher) Hash(plain string) (string, error) {
	it, sl, kl := h.params()
	salt := make([]byte, sl)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, h.input(plain), salt, it, kl)
	if err != nil {
		return "", err
	}
	enc := base64.RawStdEncoding
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", it, enc.EncodeToString(salt), enc.EncodeToString(key)), nil
}

type pbkdf2Parsed struct {
	iter       int
	salt, hash []byte
}

func parsePBKDF2(s string) (pbkdf2Parsed, bool) {
	parts := strings.Split(s, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return pbkdf2Parsed{}, false
	}
	it, err := strconv.Atoi(parts[1])
	salt, err2 := base64.RawStdEncoding.DecodeString(parts[2])
	hash, err3 := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil || err2 != nil || err3 != nil || it <= 0 || len(hash) == 0 {
		return pbkdf2Parsed{}, false
	}
	return pbkdf2Parsed{iter: it, salt: salt, hash: hash}, true
}

// Verify checks plain against hash in constant time. Unknown formats return false, nil.
func (h PBKDF2Hasher) Verify(plain, hash string) (bool, error) {
	p, ok := parsePBKDF2(hash)
	if !ok {
		return false, nil
	}
	key, err := pbkdf2.Key(sha256.New, h.input(plain), p.salt, p.iter, len(p.hash))
	if err != nil {
		return false, err
	}
	return subtle.ConstantTimeCompare(key, p.hash) == 1, nil
}

// NeedsRehash reports hashes weaker than the current parameters (or of another format).
func (h PBKDF2Hasher) NeedsRehash(hash string) bool {
	it, _, kl := h.params()
	p, ok := parsePBKDF2(hash)
	return !ok || p.iter < it || len(p.hash) != kl
}

// ChainedHasher hashes with Primary and verifies with Primary then Legacy hashers, so
// you can migrate algorithms (e.g. bcrypt → PBKDF2) transparently: verify, and if
// NeedsRehash, store Hash(plain) after a successful login.
type ChainedHasher struct {
	Primary app.PasswordHasher
	Legacy  []app.PasswordHasher
}

func (c ChainedHasher) Hash(plain string) (string, error) { return c.Primary.Hash(plain) }

func (c ChainedHasher) Verify(plain, hash string) (bool, error) {
	for _, h := range append([]app.PasswordHasher{c.Primary}, c.Legacy...) {
		ok, err := h.Verify(plain, hash)
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}

func (c ChainedHasher) NeedsRehash(hash string) bool { return c.Primary.NeedsRehash(hash) }

// VerifyPassword returns errs.ErrInvalidCredentials on mismatch. When the account does
// not exist pass hash == "" and a precomputed dummyHash: the comparison still runs, so
// response time does not reveal which accounts exist.
func VerifyPassword(h app.PasswordHasher, plain, hash, dummyHash string) error {
	target := hash
	if target == "" {
		target = dummyHash
	}
	ok, err := h.Verify(plain, target)
	if err != nil {
		return err
	}
	if !ok || hash == "" {
		return errs.ErrInvalidCredentials
	}
	return nil
}

/* ================================================================== */
/* Random values                                                       */
/* ================================================================== */

// Charsets for RandomString.
const (
	Numeric      = "0123456789"
	Alpha        = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	Alphanumeric = Alpha + Numeric
	// Readable excludes 0/O/1/I/L: safe to read aloud or type from paper.
	Readable = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"
)

// RandomString returns n characters from charset using rejection sampling (no modulo bias).
func RandomString(n int, charset string) string {
	if len(charset) < 2 || len(charset) > 256 {
		panic("security: charset must contain 2..256 bytes")
	}
	limit := 256 - (256 % len(charset)) // reject bytes >= limit
	out := make([]byte, 0, n)
	buf := make([]byte, n+n/4+8)
	for len(out) < n {
		if _, err := rand.Read(buf); err != nil {
			panic("security: crypto/rand failed: " + err.Error())
		}
		for _, b := range buf {
			if int(b) < limit {
				out = append(out, charset[int(b)%len(charset)])
				if len(out) == n {
					break
				}
			}
		}
	}
	return string(out)
}

// OTP returns a numeric one-time code.
func OTP(digits int) string { return RandomString(digits, Numeric) }

// ReadableCode returns a code like "K7QM-9XPA" (groups of 4).
func ReadableCode(n int) string {
	raw := RandomString(n, Readable)
	var b strings.Builder
	for i, c := range raw {
		if i > 0 && i%4 == 0 {
			b.WriteByte('-')
		}
		b.WriteRune(c)
	}
	return b.String()
}

// Token returns a URL-safe random token of `bytes` entropy bytes (32 = 256 bits).
func Token(bytes int) string {
	b := make([]byte, bytes)
	if _, err := rand.Read(b); err != nil {
		panic("security: crypto/rand failed: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// OpaqueToken returns a token to give to the user and its SHA-256 hash to store
// (password-reset links, API keys, refresh tokens). Look tokens up by hashing input.
func OpaqueToken() (token, hash string) {
	token = Token(32)
	return token, SHA256Hex(token)
}

/* ================================================================== */
/* Hashing / signatures                                                */
/* ================================================================== */

// SHA256Hex returns the hex SHA-256 of s.
func SHA256Hex(s string) string { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }

// HMACSHA256Hex returns the hex HMAC-SHA256 of data.
func HMACSHA256Hex(secret, data []byte) string {
	m := hmac.New(sha256.New, secret)
	m.Write(data)
	return hex.EncodeToString(m.Sum(nil))
}

// SafeEqual compares strings in constant time.
func SafeEqual(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

// VerifySignature checks "sha256=<hex>" style webhook signatures.
func VerifySignature(secret, payload []byte, signature, prefix string) bool {
	return SafeEqual(prefix+HMACSHA256Hex(secret, payload), signature)
}
