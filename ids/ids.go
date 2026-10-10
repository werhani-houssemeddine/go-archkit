// Package ids generates identifiers: random UUIDv4 and time-ordered UUIDv7 (RFC 9562),
// with a Generator interface so tests can use deterministic ids.
package ids

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Generator produces new identifiers. Inject it instead of calling NewV7 directly in
// domain code that needs deterministic tests.
type Generator interface {
	NewID() string
}

// UUID is a 16-byte RFC 9562 identifier.
type UUID [16]byte

// Nil is the zero UUID.
var Nil UUID

func (u UUID) String() string {
	var buf [36]byte
	hex.Encode(buf[0:8], u[0:4])
	buf[8] = '-'
	hex.Encode(buf[9:13], u[4:6])
	buf[13] = '-'
	hex.Encode(buf[14:18], u[6:8])
	buf[18] = '-'
	hex.Encode(buf[19:23], u[8:10])
	buf[23] = '-'
	hex.Encode(buf[24:], u[10:])
	return string(buf[:])
}

// Version returns the UUID version nibble.
func (u UUID) Version() int { return int(u[6] >> 4) }

// Time returns the creation time of a v7 UUID (zero time for other versions).
func (u UUID) Time() time.Time {
	if u.Version() != 7 {
		return time.Time{}
	}
	ms := int64(u[0])<<40 | int64(u[1])<<32 | int64(u[2])<<24 | int64(u[3])<<16 | int64(u[4])<<8 | int64(u[5])
	return time.UnixMilli(ms)
}

// MarshalText implements encoding.TextMarshaler (JSON as a string).
func (u UUID) MarshalText() ([]byte, error) { return []byte(u.String()), nil }

// UnmarshalText implements encoding.TextUnmarshaler.
func (u *UUID) UnmarshalText(b []byte) error {
	p, err := Parse(string(b))
	if err != nil {
		return err
	}
	*u = p
	return nil
}

// Parse parses the canonical 36-character form.
func Parse(s string) (UUID, error) {
	var u UUID
	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return u, fmt.Errorf("ids: invalid UUID %q", s)
	}
	j := 0
	for i := 0; i < 36; {
		if s[i] == '-' {
			i++
			continue
		}
		b, err := strconv.ParseUint(s[i:i+2], 16, 8)
		if err != nil {
			return Nil, fmt.Errorf("ids: invalid UUID %q", s)
		}
		u[j] = byte(b)
		j++
		i += 2
	}
	return u, nil
}

// IsValid reports whether s is a canonical UUID of version 1-8 with the RFC variant (or the nil UUID).
func IsValid(s string) bool {
	u, err := Parse(s)
	if err != nil {
		return false
	}
	if u == Nil {
		return true
	}
	v := u.Version()
	return v >= 1 && v <= 8 && u[8]&0xc0 == 0x80
}

// NewV4 returns a random UUID.
func NewV4() UUID {
	var u UUID
	mustRead(u[:])
	u[6] = (u[6] & 0x0f) | 0x40
	u[8] = (u[8] & 0x3f) | 0x80
	return u
}

var v7 struct {
	sync.Mutex
	lastMs int64
	seq    uint16
}

// NewV7 returns a time-ordered UUID: sortable by creation time, monotonic within the
// process even for ids created in the same millisecond. Best default for primary keys.
func NewV7() UUID {
	now := time.Now().UnixMilli()
	v7.Lock()
	if now <= v7.lastMs {
		v7.seq++
		if v7.seq > 0x0fff {
			v7.lastMs++
			v7.seq = 0
		}
		now = v7.lastMs
	} else {
		v7.lastMs = now
		var b [2]byte
		mustRead(b[:])
		v7.seq = (uint16(b[0])<<8 | uint16(b[1])) & 0x03ff
	}
	seq := v7.seq
	v7.Unlock()

	var u UUID
	mustRead(u[8:])
	u[0], u[1], u[2], u[3], u[4], u[5] = byte(now>>40), byte(now>>32), byte(now>>24), byte(now>>16), byte(now>>8), byte(now)
	u[6] = 0x70 | byte(seq>>8)&0x0f
	u[7] = byte(seq)
	u[8] = (u[8] & 0x3f) | 0x80
	return u
}

func mustRead(b []byte) {
	if _, err := rand.Read(b); err != nil {
		panic("ids: crypto/rand failed: " + err.Error())
	}
}

// V7 is a Generator producing UUIDv7 strings.
type V7 struct{}

func (V7) NewID() string { return NewV7().String() }

// V4 is a Generator producing UUIDv4 strings.
type V4 struct{}

func (V4) NewID() string { return NewV4().String() }

// Sequential is a deterministic Generator for tests: "id-1", "id-2", ...
type Sequential struct {
	Prefix string
	n      atomic.Int64
}

func (s *Sequential) NewID() string {
	p := s.Prefix
	if p == "" {
		p = "id-"
	}
	return p + strconv.FormatInt(s.n.Add(1), 10)
}
