package store

import (
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestOpenRejectsDSNWithoutTCPAndNamesTheSecret(t *testing.T) {
	_, err := Open("user:pass@db.example.com:3306/proxy", 100, 10, time.Second, 30, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil {
		t.Fatal("expected an error for a DSN without tcp(...)")
	}
	for _, want := range []string{"MySql--ConnectionString", "tcp(host:3306)"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}

func TestOpenAcceptsTCPDSN(t *testing.T) {
	s, err := Open("user:pass@tcp(db.example.com:3306)/proxy?tls=preferred", 100, 10, time.Second, 30, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("valid DSN rejected: %v", err)
	}
	_ = s.db.Close()
}
