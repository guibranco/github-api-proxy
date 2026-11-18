package identity

import (
	"encoding/base64"
	"net/http/httptest"
	"testing"
)

func TestFromRequest(t *testing.T) {
	cases := []struct{ header, kind string }{
		{"token ghp_abc", KindClassicPAT},
		{"Bearer github_pat_abc", KindFineGrained},
		{"bearer ghs_abc", KindInstallation},
		{"Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:ghu_abc")), KindUserToServer},
		{"Bearer gho_abc", KindOAuth},
		{"Bearer something", KindOther},
		{"", KindNone},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/", nil)
		if c.header != "" {
			r.Header.Set("Authorization", c.header)
		}
		got := FromRequest(r)
		if got.Kind != c.kind {
			t.Errorf("%q: kind %s want %s", c.header, got.Kind, c.kind)
		}
		if c.kind != KindNone && len(got.Hash) != 64 {
			t.Errorf("%q: missing hash", c.header)
		}
	}
	a := httptest.NewRequest("GET", "/", nil)
	a.Header.Set("Authorization", "token ghp_same")
	b := httptest.NewRequest("GET", "/", nil)
	b.Header.Set("Authorization", "Bearer ghp_same")
	if FromRequest(a).Hash != FromRequest(b).Hash {
		t.Fatal("same token under different schemes must share a fingerprint")
	}
}

func TestAppIDFromJWT(t *testing.T) {
	enc := base64.RawURLEncoding.EncodeToString
	tok := enc([]byte(`{"alg":"RS256"}`)) + "." + enc([]byte(`{"iss":123456,"iat":1}`)) + ".sig"
	if KindOf(tok) != KindAppJWT {
		t.Fatalf("kind %s", KindOf(tok))
	}
	if got := AppIDFromJWT(tok); got != "123456" {
		t.Fatalf("got %q", got)
	}
	tok = enc([]byte(`{}`)) + "." + enc([]byte(`{"iss":"Iv1.abc"}`)) + ".sig"
	if got := AppIDFromJWT(tok); got != "Iv1.abc" {
		t.Fatalf("got %q", got)
	}
}
