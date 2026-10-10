package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/werhani-houssemeddine/go-archkit/config"
	"github.com/werhani-houssemeddine/go-archkit/errs"
)

type Config struct {
	Env         string        `env:"APP_ENV" default:"development" oneof:"development,staging,production"`
	Port        int           `env:"PORT" default:"8080" min:"1" max:"65535"`
	Debug       bool          `env:"DEBUG" default:"false"`
	DatabaseURL string        `env:"DATABASE_URL" required:"true" url:"postgres,postgresql"`
	JWTSecret   string        `env:"JWT_SECRET" required:"true" min:"32" secret:"true"`
	Timeout     time.Duration `env:"HTTP_TIMEOUT" default:"15s"`
	Origins     []string      `env:"CORS_ORIGINS"`
	DB          struct {
		MaxConns int `env:"MAX_CONNS" default:"10"`
	} `prefix:"DB_"`
}

func TestLoad(t *testing.T) {
	var c Config
	err := config.Load(&c, config.Options{Lookup: config.MapLookup(map[string]string{
		"PORT": "9000", "DEBUG": "yes", "DATABASE_URL": "postgres://u:p@h/db", "JWT_SECRET": strings.Repeat("x", 32),
		"CORS_ORIGINS": "https://a.io, https://b.io", "DB_MAX_CONNS": "25", "APP_ENV": "",
	})})
	if err != nil {
		t.Fatal(err)
	}
	if c.Env != "development" || c.Port != 9000 || !c.Debug || c.Timeout != 15*time.Second || len(c.Origins) != 2 || c.DB.MaxConns != 25 {
		t.Fatalf("got %+v", c)
	}
	if config.Describe(c)["JWT_SECRET"] != "***" {
		t.Fatal("secret must be masked")
	}
}

func TestLoadReportsEveryProblem(t *testing.T) {
	var c Config
	err := config.Load(&c, config.Options{Lookup: config.MapLookup(map[string]string{"PORT": "abc", "APP_ENV": "prod", "JWT_SECRET": "short"})})
	e, ok := errs.As(err)
	if !ok || e.Code != "INVALID_CONFIG" {
		t.Fatalf("got %v", err)
	}
	var paths []string
	for _, is := range e.Issues {
		paths = append(paths, is.Path)
	}
	if strings.Join(paths, ",") != "APP_ENV,PORT,DATABASE_URL,JWT_SECRET" {
		t.Fatalf("paths %v\n%s", paths, e.Message)
	}
}
