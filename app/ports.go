// Package app defines application-layer ports: the interfaces use cases depend on.
// Infrastructure packages (pg, security, jwt, mail, cache, events) implement them and
// testkit provides fakes. Nothing here imports a driver or framework.
package app

import (
	"context"
	"time"

	"github.com/werhani-houssemeddine/go-archkit/domain"
)

// UseCase is one application operation ("PlaceOrder"). Optional: plain functions or
// cqrs handlers work equally well.
type UseCase[I, O any] interface {
	Execute(ctx context.Context, in I) (O, error)
}

// UseCaseFunc adapts a function to UseCase.
type UseCaseFunc[I, O any] func(ctx context.Context, in I) (O, error)

func (f UseCaseFunc[I, O]) Execute(ctx context.Context, in I) (O, error) { return f(ctx, in) }

/* ---------------- Persistence ---------------- */

// IsolationLevel for transactions.
type IsolationLevel int

const (
	IsolationDefault IsolationLevel = iota
	ReadCommitted
	RepeatableRead
	Serializable
)

// TxOptions configure a transaction.
type TxOptions struct {
	Isolation IsolationLevel
	ReadOnly  bool
}

// UnitOfWork runs work atomically. Repositories built on the same infrastructure join
// the transaction carried by ctx, so use cases stay persistence-agnostic:
//
//	err := uow.Transact(ctx, func(ctx context.Context) error {
//	    if err := accounts.Save(ctx, from); err != nil { return err }
//	    return accounts.Save(ctx, to)
//	})
type UnitOfWork interface {
	Transact(ctx context.Context, fn func(ctx context.Context) error, opts ...TxOptions) error
}

/* ---------------- Events ---------------- */

// EventHandler handles a domain/integration event.
type EventHandler func(ctx context.Context, e domain.Event) error

// EventPublisher publishes events.
type EventPublisher interface {
	Publish(ctx context.Context, events ...domain.Event) error
}

// EventSubscriber registers handlers; "*" receives every event. Returns an unsubscribe func.
type EventSubscriber interface {
	Subscribe(eventType string, name string, h EventHandler) (unsubscribe func())
}

// EventBus is both.
type EventBus interface {
	EventPublisher
	EventSubscriber
}

/* ---------------- Security ---------------- */

// PasswordHasher hashes and verifies passwords.
type PasswordHasher interface {
	Hash(plain string) (string, error)
	Verify(plain, hash string) (bool, error)
	// NeedsRehash reports hashes made with outdated parameters (upgrade them at login).
	NeedsRehash(hash string) bool
}

// Encryptor provides authenticated symmetric encryption. aad binds a ciphertext to its
// context (e.g. "user:42") and may be nil.
type Encryptor interface {
	Encrypt(plaintext, aad []byte) (string, error)
	Decrypt(ciphertext string, aad []byte) ([]byte, error)
}

// Claims are the verified contents of a token. Subject is required.
type Claims struct {
	Subject     string         `json:"sub"`
	Roles       []string       `json:"roles,omitempty"`
	Permissions []string       `json:"permissions,omitempty"`
	TenantID    string         `json:"tenant_id,omitempty"`
	Extra       map[string]any `json:"-"`
	IssuedAt    time.Time      `json:"-"`
	ExpiresAt   time.Time      `json:"-"`
	TokenID     string         `json:"-"`
}

// TokenIssuer signs tokens.
type TokenIssuer interface {
	Issue(ctx context.Context, c Claims, ttl time.Duration) (string, error)
}

// TokenVerifier verifies tokens and returns errs.ErrExpiredToken / errs.ErrInvalidToken on failure.
type TokenVerifier interface {
	Verify(ctx context.Context, token string) (Claims, error)
}

// TokenService both issues and verifies.
type TokenService interface {
	TokenIssuer
	TokenVerifier
}

/* ---------------- Messaging ---------------- */

// MailAddress is an email address with optional display name.
type MailAddress struct {
	Name    string
	Address string
}

// Attachment is a file attached to an email.
type Attachment struct {
	Filename    string
	ContentType string
	Data        []byte
}

// MailMessage is a transport-agnostic email.
type MailMessage struct {
	From        *MailAddress // nil = mailer default
	To, Cc, Bcc []MailAddress
	ReplyTo     *MailAddress
	Subject     string
	Text, HTML  string
	Headers     map[string]string
	Attachments []Attachment
}

// Mailer sends emails.
type Mailer interface {
	Send(ctx context.Context, m MailMessage) error
}

/* ---------------- Cache ---------------- */

// Cache stores byte values with optional TTL. Typed helpers live in package cache.
type Cache interface {
	Get(ctx context.Context, key string) ([]byte, bool, error)
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
	Delete(ctx context.Context, keys ...string) error
	DeletePrefix(ctx context.Context, prefix string) (int, error)
}
