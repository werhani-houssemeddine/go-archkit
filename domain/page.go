package domain

import (
	"encoding/base64"
	"encoding/json"
	"slices"
	"strings"

	"github.com/werhani-houssemeddine/go-archkit/errs"
)

// SortOrder is one sort key.
type SortOrder struct {
	Field string `json:"field"`
	Desc  bool   `json:"desc"`
}

// PageRequest asks for a 1-based page.
type PageRequest struct {
	Page int         `json:"page"`
	Size int         `json:"size"`
	Sort []SortOrder `json:"sort,omitempty"`
}

// Offset returns the SQL offset.
func (r PageRequest) Offset() int { return (r.Page - 1) * r.Size }

// NewPageRequest clamps untrusted input: page >= 1, 1 <= size <= maxSize.
func NewPageRequest(page, size, defaultSize, maxSize int) PageRequest {
	if page < 1 {
		page = 1
	}
	if size <= 0 {
		size = defaultSize
	}
	if size > maxSize {
		size = maxSize
	}
	return PageRequest{Page: page, Size: size}
}

// Page is one page of results.
type Page[T any] struct {
	Items      []T  `json:"items"`
	Page       int  `json:"page"`
	Size       int  `json:"size"`
	Total      int  `json:"total"`
	TotalPages int  `json:"totalPages"`
	HasNext    bool `json:"hasNext"`
	HasPrev    bool `json:"hasPrevious"`
}

// NewPage builds a page from items and the total count.
func NewPage[T any](items []T, total int, req PageRequest) Page[T] {
	pages := 1
	if req.Size > 0 && total > 0 {
		pages = (total + req.Size - 1) / req.Size
	}
	if items == nil {
		items = []T{}
	}
	return Page[T]{Items: items, Page: req.Page, Size: req.Size, Total: total, TotalPages: pages, HasNext: req.Page < pages, HasPrev: req.Page > 1}
}

// MapPage converts the items of a page.
func MapPage[T, U any](p Page[T], fn func(T) U) Page[U] {
	out := make([]U, len(p.Items))
	for i, it := range p.Items {
		out[i] = fn(it)
	}
	return Page[U]{Items: out, Page: p.Page, Size: p.Size, Total: p.Total, TotalPages: p.TotalPages, HasNext: p.HasNext, HasPrev: p.HasPrev}
}

// ParseSort parses "name,-createdAt" and rejects fields not in allowed (prevents sorting
// on arbitrary or unindexed columns).
func ParseSort(raw string, allowed ...string) ([]SortOrder, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var out []SortOrder
	for _, part := range strings.Split(raw, ",") {
		p := strings.TrimSpace(part)
		desc := strings.HasPrefix(p, "-")
		field := strings.TrimLeft(p, "-+")
		if !slices.Contains(allowed, field) {
			return nil, errs.InvalidField("sort", "invalid_sort", "cannot sort by '"+field+"'; allowed: "+strings.Join(allowed, ", "))
		}
		out = append(out, SortOrder{Field: field, Desc: desc})
	}
	return out, nil
}

// CursorPage is a keyset-paginated page.
type CursorPage[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"nextCursor,omitempty"`
	HasMore    bool   `json:"hasMore"`
}

// EncodeCursor turns a keyset value into an opaque cursor.
func EncodeCursor(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// DecodeCursor parses a cursor produced by EncodeCursor.
func DecodeCursor(cursor string, into any) error {
	b, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil || json.Unmarshal(b, into) != nil {
		return errs.InvalidField("cursor", "invalid_cursor", "invalid cursor")
	}
	return nil
}
