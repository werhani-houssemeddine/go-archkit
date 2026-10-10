// Package mail implements app.Mailer over SMTP (standard library only) and provides a
// logging mailer for development and HTML templating helpers.
package mail

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/werhani-houssemeddine/go-archkit/app"
	"github.com/werhani-houssemeddine/go-archkit/errs"
	"github.com/werhani-houssemeddine/go-archkit/ids"
	"github.com/werhani-houssemeddine/go-archkit/logx"
)

// SMTPOptions configure the SMTP mailer.
type SMTPOptions struct {
	Host     string
	Port     int // 587 (STARTTLS, default) or 465 (implicit TLS)
	Username string
	Password string
	From     app.MailAddress // default sender
	// ImplicitTLS uses TLS from the first byte (port 465).
	ImplicitTLS bool
	// AllowInsecure permits plain-text SMTP (local test servers only).
	AllowInsecure bool
	Timeout       time.Duration // default 15s
	TLSConfig     *tls.Config
	Logger        *slog.Logger
}

// SMTP sends mail through any SMTP server (provider-agnostic; replaces Gmail-only helpers).
type SMTP struct {
	o   SMTPOptions
	log *slog.Logger
}

var _ app.Mailer = (*SMTP)(nil)

// NewSMTP creates the mailer.
func NewSMTP(o SMTPOptions) *SMTP {
	if o.Port == 0 {
		o.Port = 587
	}
	if o.Timeout <= 0 {
		o.Timeout = 15 * time.Second
	}
	return &SMTP{o: o, log: logx.OrDiscard(o.Logger)}
}

// Send delivers m.
func (s *SMTP) Send(ctx context.Context, m app.MailMessage) error {
	if m.From == nil {
		m.From = &s.o.From
	}
	raw, err := Build(m)
	if err != nil {
		return err
	}
	var rcpt []string
	for _, l := range [][]app.MailAddress{m.To, m.Cc, m.Bcc} {
		for _, a := range l {
			rcpt = append(rcpt, a.Address)
		}
	}
	if err := s.deliver(ctx, m.From.Address, rcpt, raw); err != nil {
		return errs.Wrap(err, errs.KindInfrastructure, "MAIL_SEND_FAILED", "failed to send email")
	}
	s.log.InfoContext(ctx, "mail sent", slog.String("subject", m.Subject), slog.Int("recipients", len(rcpt)))
	return nil
}

func (s *SMTP) deliver(ctx context.Context, from string, to []string, msg []byte) error {
	addr := net.JoinHostPort(s.o.Host, fmt.Sprint(s.o.Port))
	d := net.Dialer{Timeout: s.o.Timeout}
	tlsCfg := s.o.TLSConfig
	if tlsCfg == nil {
		tlsCfg = &tls.Config{ServerName: s.o.Host, MinVersion: tls.VersionTLS12}
	}
	var conn net.Conn
	var err error
	if s.o.ImplicitTLS {
		conn, err = (&tls.Dialer{NetDialer: &d, Config: tlsCfg}).DialContext(ctx, "tcp", addr)
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return err
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	} else {
		_ = conn.SetDeadline(time.Now().Add(s.o.Timeout))
	}
	c, err := smtp.NewClient(conn, s.o.Host)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()
	if !s.o.ImplicitTLS {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(tlsCfg); err != nil {
				return err
			}
		} else if !s.o.AllowInsecure {
			return fmt.Errorf("smtp server does not support STARTTLS")
		}
	}
	if s.o.Username != "" {
		if err := c.Auth(smtp.PlainAuth("", s.o.Username, s.o.Password, s.o.Host)); err != nil {
			return err
		}
	}
	if err := c.Mail(from); err != nil {
		return err
	}
	for _, r := range to {
		if err := c.Rcpt(r); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

var crlfRe = regexp.MustCompile(`[\r\n]`)

func formatAddr(a app.MailAddress) (string, error) {
	if _, err := mail.ParseAddress(a.Address); err != nil || crlfRe.MatchString(a.Address) {
		return "", errs.InvalidField("email", "invalid_email", "invalid email address: "+a.Address)
	}
	return (&mail.Address{Name: crlfRe.ReplaceAllString(a.Name, " "), Address: a.Address}).String(), nil
}

func formatList(l []app.MailAddress) (string, error) {
	parts := make([]string, 0, len(l))
	for _, a := range l {
		s, err := formatAddr(a)
		if err != nil {
			return "", err
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, ", "), nil
}

// Build renders a MIME message (multipart/alternative text+HTML, attachments).
// Header values are sanitized against CRLF injection.
func Build(m app.MailMessage) ([]byte, error) {
	if len(m.To)+len(m.Cc)+len(m.Bcc) == 0 {
		return nil, errs.InvalidField("to", "required", "at least one recipient is required")
	}
	if m.Text == "" && m.HTML == "" {
		return nil, errs.InvalidField("body", "required", "text or html body is required")
	}
	if m.Text == "" {
		m.Text = HTMLToText(m.HTML)
	}
	var buf bytes.Buffer
	hdr := func(k, v string) { fmt.Fprintf(&buf, "%s: %s\r\n", k, crlfRe.ReplaceAllString(v, " ")) }
	from, err := formatAddr(*m.From)
	if err != nil {
		return nil, err
	}
	hdr("From", from)
	if to, err := formatList(m.To); err != nil {
		return nil, err
	} else if to != "" {
		hdr("To", to)
	}
	if cc, err := formatList(m.Cc); err != nil {
		return nil, err
	} else if cc != "" {
		hdr("Cc", cc)
	}
	if m.ReplyTo != nil {
		rt, err := formatAddr(*m.ReplyTo)
		if err != nil {
			return nil, err
		}
		hdr("Reply-To", rt)
	}
	hdr("Subject", mime.QEncoding.Encode("utf-8", m.Subject))
	hdr("Date", time.Now().Format(time.RFC1123Z))
	domain := "localhost"
	if at := strings.LastIndexByte(m.From.Address, '@'); at >= 0 {
		domain = m.From.Address[at+1:]
	}
	hdr("Message-ID", "<"+ids.NewV7().String()+"@"+domain+">")
	hdr("MIME-Version", "1.0")
	keys := make([]string, 0, len(m.Headers))
	for k := range m.Headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		hdr(textproto.CanonicalMIMEHeaderKey(crlfRe.ReplaceAllString(k, "")), m.Headers[k])
	}

	mixed := multipart.NewWriter(&buf)
	hdr("Content-Type", `multipart/mixed; boundary="`+mixed.Boundary()+`"`)
	buf.WriteString("\r\n")

	altBuf := &bytes.Buffer{}
	alt := multipart.NewWriter(altBuf)
	writePart := func(ct, body string) error {
		w, err := alt.CreatePart(textproto.MIMEHeader{"Content-Type": {ct + "; charset=utf-8"}, "Content-Transfer-Encoding": {"quoted-printable"}})
		if err != nil {
			return err
		}
		qp := quotedprintable.NewWriter(w)
		if _, err := qp.Write([]byte(body)); err != nil {
			return err
		}
		return qp.Close()
	}
	if err := writePart("text/plain", m.Text); err != nil {
		return nil, err
	}
	if m.HTML != "" {
		if err := writePart("text/html", m.HTML); err != nil {
			return nil, err
		}
	}
	_ = alt.Close()
	pw, err := mixed.CreatePart(textproto.MIMEHeader{"Content-Type": {`multipart/alternative; boundary="` + alt.Boundary() + `"`}})
	if err != nil {
		return nil, err
	}
	_, _ = pw.Write(altBuf.Bytes())

	for _, a := range m.Attachments {
		ct := a.ContentType
		if ct == "" {
			ct = "application/octet-stream"
		}
		name := mime.QEncoding.Encode("utf-8", crlfRe.ReplaceAllString(a.Filename, "_"))
		w, err := mixed.CreatePart(textproto.MIMEHeader{
			"Content-Type":              {ct + `; name="` + name + `"`},
			"Content-Disposition":       {`attachment; filename="` + name + `"`},
			"Content-Transfer-Encoding": {"base64"},
		})
		if err != nil {
			return nil, err
		}
		enc := base64.StdEncoding.EncodeToString(a.Data)
		for len(enc) > 76 {
			_, _ = w.Write([]byte(enc[:76] + "\r\n"))
			enc = enc[76:]
		}
		_, _ = w.Write([]byte(enc + "\r\n"))
	}
	_ = mixed.Close()
	return buf.Bytes(), nil
}

// LogMailer only logs messages (local development).
type LogMailer struct{ Logger *slog.Logger }

func (l LogMailer) Send(ctx context.Context, m app.MailMessage) error {
	to := make([]string, len(m.To))
	for i, a := range m.To {
		to[i] = a.Address
	}
	logx.OrDiscard(l.Logger).InfoContext(ctx, "mail (not sent)", slog.Any("to", to), slog.String("subject", m.Subject))
	return nil
}

// Templates renders named html/template templates (auto-escaped) into subject/body.
//
//	t, err := mail.ParseTemplates(templatesFS, "templates/*.html")
//	html, err := t.Render("welcome.html", map[string]any{"Name": user.Name})
type Templates struct{ t *template.Template }

// ParseTemplates parses templates from an fs.FS (e.g. embed.FS) matching patterns.
func ParseTemplates(fsys fs.FS, patterns ...string) (*Templates, error) {
	t, err := template.ParseFS(fsys, patterns...)
	if err != nil {
		return nil, err
	}
	return &Templates{t: t}, nil
}

// NewTemplates wraps parsed templates.
func NewTemplates(t *template.Template) *Templates { return &Templates{t: t} }

// Render executes the named template.
func (t *Templates) Render(name string, data any) (string, error) {
	var b bytes.Buffer
	if err := t.t.ExecuteTemplate(&b, name, data); err != nil {
		return "", err
	}
	return b.String(), nil
}

var (
	scriptRe = regexp.MustCompile(`(?is)<(script|style)[^>]*>.*?</(script|style)>`)
	brRe     = regexp.MustCompile(`(?i)<br\s*/?>`)
	blockRe  = regexp.MustCompile(`(?i)</(p|div|h[1-6]|li|tr)>`)
	tagRe    = regexp.MustCompile(`<[^>]+>`)
	nlRe     = regexp.MustCompile(`\n{3,}`)
)

// HTMLToText produces a plain-text fallback from HTML.
func HTMLToText(html string) string {
	s := scriptRe.ReplaceAllString(html, "")
	s = brRe.ReplaceAllString(s, "\n")
	s = blockRe.ReplaceAllString(s, "\n")
	s = tagRe.ReplaceAllString(s, "")
	s = strings.NewReplacer("&nbsp;", " ", "&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`, "&#39;", "'").Replace(s)
	return strings.TrimSpace(nlRe.ReplaceAllString(s, "\n\n"))
}
