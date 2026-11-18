package config

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func fakeGrimoire(t *testing.T, configs map[string]string, secrets map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "app-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Query().Get("environment") != "production" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"detail":"Environment not found."}`))
			return
		}
		switch {
		case r.URL.Path == "/consumer/configurations":
			items := []map[string]string{}
			for k, v := range configs {
				items = append(items, map[string]string{"key": k, "value": v, "label": "production"})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
		case strings.HasPrefix(r.URL.Path, "/consumer/secrets/"):
			name := strings.TrimPrefix(r.URL.Path, "/consumer/secrets/")
			v, ok := secrets[name]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"detail":"Secret not found."}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"name": name, "value": v})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestLoadFromGrimoire(t *testing.T) {
	srv := fakeGrimoire(t, map[string]string{
		"Network:AllowedSources": "node1.example.com, 198.51.100.30",
		"LogStream:Server":       "https://api.logs.example.com",
		"LogStream:ApiKey":       "github-api-proxy",
		"Storage:RetentionDays":  "45",
	}, map[string]string{
		"MySql--ConnectionString": "u:p@tcp(db:3306)/proxy",
		"Admin--ApiToken":         "0123456789abcdef0123456789abcdef",
		"LogStream--ApiToken":     "lss_token",
	})
	t.Setenv("Proxy__ListenAddr", "0.0.0.0:9000") // env overrides Grimoire

	cfg, client, err := Load(context.Background(), Bootstrap{BaseURL: srv.URL, APIKey: "app-key", Environment: "production"}, "1.4.2", "test")
	if err != nil {
		t.Fatal(err)
	}
	if client == nil {
		t.Fatal("expected a Grimoire client")
	}
	if cfg.MySQLDSN != "u:p@tcp(db:3306)/proxy" || cfg.AdminAPIToken == "" || cfg.RetentionDays != 45 {
		t.Fatalf("secrets/configs not applied: %+v", cfg)
	}
	if cfg.ListenAddr != "0.0.0.0:9000" {
		t.Fatalf("env override ignored: %s", cfg.ListenAddr)
	}
	ls := cfg.LogStream
	if !ls.Ready() || ls.AppID != "1.4.2" || ls.Transport != "udp" || ls.UDPHost != "api.logs.example.com" || ls.UDPPort != 8082 || ls.APIToken != "lss_token" {
		t.Fatalf("bad LogStream config: %+v", ls)
	}
	if !strings.Contains(cfg.Origin, "3 secrets") {
		t.Fatalf("origin: %s", cfg.Origin)
	}
}

func TestLoadFailsWhenGrimoireRejects(t *testing.T) {
	srv := fakeGrimoire(t, nil, nil)
	if _, _, err := Load(context.Background(), Bootstrap{BaseURL: srv.URL, APIKey: "wrong", Environment: "production"}, "1.0.0", "t"); err == nil ||
		!strings.Contains(err.Error(), "rejected the API key") {
		t.Fatalf("expected key rejection, got %v", err)
	}
	if _, _, err := Load(context.Background(), Bootstrap{BaseURL: srv.URL, APIKey: "app-key", Environment: "staging"}, "1.0.0", "t"); err == nil ||
		!strings.Contains(err.Error(), "does not know the environment") {
		t.Fatalf("expected unknown environment, got %v", err)
	}
}

func TestLoadRequiresSecretsAndAllowList(t *testing.T) {
	srv := fakeGrimoire(t, map[string]string{}, map[string]string{})
	_, _, err := Load(context.Background(), Bootstrap{BaseURL: srv.URL, APIKey: "app-key", Environment: "production"}, "1.0.0", "t")
	if err == nil {
		t.Fatal("expected validation errors")
	}
	for _, want := range []string{"MySql:ConnectionString", "Admin:ApiToken", "Network:AllowedSources"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in %v", want, err)
		}
	}
}

func TestLogStreamAppIDOverride(t *testing.T) {
	cfg, _ := build(settings{"logstream:appid": "custom", "logstream:server": "https://x.example.com"}, "9.9.9")
	if cfg.LogStream.AppID != "custom" {
		t.Fatalf("got %s", cfg.LogStream.AppID)
	}
	if cfg.LogStream.Ready() {
		t.Fatal("not ready without key and token")
	}
}
