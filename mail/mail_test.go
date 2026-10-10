package mail_test

import (
	"bufio"
	"context"
	"net"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/werhani-houssemeddine/go-archkit/app"
	"github.com/werhani-houssemeddine/go-archkit/mail"
)

func TestBuildSanitizesHeaders(t *testing.T) {
	raw, err := mail.Build(app.MailMessage{
		From:        &app.MailAddress{Name: "Acme", Address: "no-reply@acme.test"},
		To:          []app.MailAddress{{Name: "Ali\r\nBcc: victim@x.io", Address: "ali@x.io"}},
		Subject:     "Bonjour été\r\nX-Injected: 1",
		HTML:        "<p>Hello</p><p>World &amp; co</p>",
		Attachments: []app.Attachment{{Filename: "a.txt", Data: []byte("hi")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	if strings.Contains(s, "\r\nBcc:") || strings.Contains(s, "\r\nX-Injected") {
		t.Fatalf("header injection:\n%s", s)
	}
	if !strings.Contains(s, "multipart/alternative") || !strings.Contains(s, "text/plain") || !strings.Contains(s, "attachment; filename=\"a.txt\"") {
		t.Fatal("mime structure")
	}
	if mail.HTMLToText("<p>Hello</p><p>World &amp; co</p>") != "Hello\nWorld & co" {
		t.Fatal("text fallback")
	}
	if _, err := mail.Build(app.MailMessage{From: &app.MailAddress{Address: "a@b.c"}, Subject: "x", Text: "x"}); err == nil {
		t.Fatal("recipients required")
	}
}

func TestTemplatesEscape(t *testing.T) {
	tpl, err := mail.ParseTemplates(fstest.MapFS{"welcome.html": {Data: []byte(`<p>Hi {{.Name}}</p>`)}}, "*.html")
	if err != nil {
		t.Fatal(err)
	}
	out, _ := tpl.Render("welcome.html", map[string]string{"Name": "<script>"})
	if out != "<p>Hi &lt;script&gt;</p>" {
		t.Fatal(out)
	}
}

// Minimal SMTP server to exercise the real client path (plain, AllowInsecure).
func TestSMTPSend(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		r := bufio.NewReader(c)
		w := func(s string) { _, _ = c.Write([]byte(s + "\r\n")) }
		w("220 test")
		var data strings.Builder
		inData := false
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			if inData {
				if line == ".\r\n" {
					inData = false
					w("250 queued")
					continue
				}
				data.WriteString(line)
				continue
			}
			switch cmd := strings.ToUpper(strings.TrimSpace(line)); {
			case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
				w("250 test")
			case cmd == "DATA":
				inData = true
				w("354 go")
			case cmd == "QUIT":
				w("221 bye")
				got <- data.String()
				return
			default:
				w("250 ok")
			}
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	m := mail.NewSMTP(mail.SMTPOptions{Host: "127.0.0.1", Port: port, AllowInsecure: true, From: app.MailAddress{Address: "no-reply@acme.test"}})
	if err := m.Send(context.Background(), app.MailMessage{To: []app.MailAddress{{Address: "a@b.test"}}, Subject: "Hi", Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	if body := <-got; !strings.Contains(body, "Subject: Hi") {
		t.Fatalf("body: %s", body)
	}
}
