package logx_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/werhani-houssemeddine/go-archkit/errs"
	"github.com/werhani-houssemeddine/go-archkit/execctx"
	"github.com/werhani-houssemeddine/go-archkit/logx"
)

func TestStructuredRedactedContextual(t *testing.T) {
	var buf bytes.Buffer
	log := logx.New(logx.Options{Output: &buf, Level: slog.LevelDebug, Attrs: []slog.Attr{slog.String("service", "api")}})
	ctx := execctx.WithActor(execctx.WithCorrelationID(context.Background(), "c-1"), &execctx.Actor{ID: "u-1"})
	log.With("module", "auth").InfoContext(ctx, "login",
		slog.Group("user", slog.String("email", "a@b.c"), slog.String("password", "hunter2")),
		slog.String("Authorization", "Bearer x"),
		logx.Err(errs.Wrap(os.ErrNotExist, errs.KindNotFound, "", "missing")))
	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatal(err, buf.String())
	}
	user := rec["user"].(map[string]any)
	errAttr := rec["err"].(map[string]any)
	if rec["service"] != "api" || rec["module"] != "auth" || rec["correlation_id"] != "c-1" || rec["actor_id"] != "u-1" ||
		user["password"] != "[REDACTED]" || user["email"] != "a@b.c" || rec["Authorization"] != "[REDACTED]" ||
		errAttr["code"] != "NOT_FOUND" || errAttr["kind"] != "not_found" {
		t.Fatalf("record: %s", buf.String())
	}
}

func TestRotatingFileAndFanout(t *testing.T) {
	dir := t.TempDir()
	rf := &logx.RotatingFile{Dir: dir, Prefix: "audit", MaxSize: 300, MaxFiles: 2}
	var console bytes.Buffer
	log := slog.New(logx.Fanout{
		logx.NewHandler(logx.Options{Output: rf}),
		logx.NewHandler(logx.Options{Output: &console, Level: slog.LevelWarn}),
	})
	for i := 0; i < 20; i++ {
		log.Info("entry", "i", i)
	}
	log.Warn("careful")
	_ = rf.Close()
	files, _ := filepath.Glob(filepath.Join(dir, "*.log"))
	if len(files) != 3 {
		t.Fatalf("files %v", files)
	}
	if strings.Count(console.String(), "\n") != 1 {
		t.Fatalf("console should only have warn+: %q", console.String())
	}
}

func TestAuditor(t *testing.T) {
	sink := &logx.MemoryAuditSink{}
	a := logx.Auditor{Sinks: []logx.AuditSink{sink}}
	ctx := execctx.WithActor(execctx.WithCorrelationID(context.Background(), "c"), &execctx.Actor{ID: "admin", TenantID: "t1"})
	if _, err := a.Record(ctx, logx.AuditRecord{Action: "user.disable", ResourceType: "user", ResourceID: "42"}); err != nil {
		t.Fatal(err)
	}
	r := sink.Records[0]
	if r.ActorID != "admin" || r.TenantID != "t1" || r.CorrelationID != "c" || r.Outcome != logx.AuditSuccess || r.ID == "" {
		t.Fatalf("%+v", r)
	}
}
