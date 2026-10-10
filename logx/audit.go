package logx

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/werhani-houssemeddine/go-archkit/clock"
	"github.com/werhani-houssemeddine/go-archkit/execctx"
	"github.com/werhani-houssemeddine/go-archkit/ids"
)

// AuditOutcome of an audited action.
type AuditOutcome string

const (
	AuditSuccess AuditOutcome = "success"
	AuditFailure AuditOutcome = "failure"
	AuditDenied  AuditOutcome = "denied"
)

// AuditRecord is one entry of the audit trail.
type AuditRecord struct {
	ID            string         `json:"id"`
	Time          time.Time      `json:"time"`
	Action        string         `json:"action"` // "user.disable"
	ResourceType  string         `json:"resourceType,omitempty"`
	ResourceID    string         `json:"resourceId,omitempty"`
	Outcome       AuditOutcome   `json:"outcome"`
	ActorID       string         `json:"actorId,omitempty"`
	TenantID      string         `json:"tenantId,omitempty"`
	CorrelationID string         `json:"correlationId,omitempty"`
	IP            string         `json:"ip,omitempty"`
	Reason        string         `json:"reason,omitempty"`
	Metadata      map[string]any `json:"metadata,omitempty"`
}

// AuditSink stores audit records (log file, database table, SIEM...).
type AuditSink interface {
	WriteAudit(ctx context.Context, r AuditRecord) error
}

// Auditor writes audit records to sinks, filling id, time, actor, tenant and correlation
// from the context. Audit is not diagnostic logging: writes must not fail silently, so
// sink errors are returned to the caller.
type Auditor struct {
	Sinks []AuditSink
	Clock clock.Clock
	IDs   ids.Generator
}

// Record writes r to every sink.
func (a *Auditor) Record(ctx context.Context, r AuditRecord) (AuditRecord, error) {
	if r.ID == "" {
		if a.IDs != nil {
			r.ID = a.IDs.NewID()
		} else {
			r.ID = ids.NewV7().String()
		}
	}
	if r.Time.IsZero() {
		c := a.Clock
		if c == nil {
			c = clock.Default
		}
		r.Time = c.Now()
	}
	if r.Outcome == "" {
		r.Outcome = AuditSuccess
	}
	if act := execctx.ActorFrom(ctx); act != nil && r.ActorID == "" {
		r.ActorID = act.ID
	}
	if r.TenantID == "" {
		r.TenantID = execctx.TenantID(ctx)
	}
	if r.CorrelationID == "" {
		r.CorrelationID = execctx.CorrelationID(ctx)
	}
	var out []error
	for _, s := range a.Sinks {
		if err := s.WriteAudit(ctx, r); err != nil {
			out = append(out, err)
		}
	}
	return r, errors.Join(out...)
}

// SlogAuditSink writes audit records through a dedicated *slog.Logger
// (typically one backed by a RotatingFile).
type SlogAuditSink struct{ Logger *slog.Logger }

func (s SlogAuditSink) WriteAudit(ctx context.Context, r AuditRecord) error {
	s.Logger.LogAttrs(ctx, slog.LevelInfo, "audit", slog.Any("audit", r))
	return nil
}

// MemoryAuditSink keeps records in memory (tests).
type MemoryAuditSink struct {
	mu      sync.Mutex
	Records []AuditRecord
}

func (m *MemoryAuditSink) WriteAudit(_ context.Context, r AuditRecord) error {
	m.mu.Lock()
	m.Records = append(m.Records, r)
	m.mu.Unlock()
	return nil
}
