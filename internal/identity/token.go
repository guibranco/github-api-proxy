// Package identity works out who is calling, from the GitHub credential the
// client already sends. The raw token is never stored or logged: only its
// SHA-256 fingerprint, its kind and the account it resolves to.
package identity

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
)

// Token kinds, derived from GitHub's documented token prefixes.
const (
	KindNone         = "none"
	KindClassicPAT   = "pat"              // ghp_
	KindFineGrained  = "fine_grained_pat" // github_pat_
	KindOAuth        = "oauth"            // gho_
	KindUserToServer = "app_user"         // ghu_
	KindInstallation = "app_installation" // ghs_
	KindAppJWT       = "app_jwt"          // eyJ... (GitHub App JWT)
	KindOther        = "other"
)

// Credential is what was extracted from a request.
type Credential struct {
	Token string // raw token; keep in memory only
	Hash  string // hex SHA-256 of the token
	Kind  string
}

// FromRequest extracts the credential from the Authorization header. It
// accepts "token X", "Bearer X" and "Basic base64(user:X)" (all used by
// GitHub clients). It returns a credential with KindNone when there is none.
func FromRequest(r *http.Request) Credential {
	h := strings.TrimSpace(r.Header.Get("Authorization"))
	if h == "" {
		return Credential{Kind: KindNone}
	}
	scheme, value, _ := strings.Cut(h, " ")
	value = strings.TrimSpace(value)
	var token string
	switch strings.ToLower(scheme) {
	case "token", "bearer":
		token = value
	case "basic":
		raw, err := base64.StdEncoding.DecodeString(value)
		if err != nil {
			token = value
			break
		}
		user, pass, ok := strings.Cut(string(raw), ":")
		if ok && pass != "" {
			token = pass
		} else {
			token = user
		}
	default:
		token = h
	}
	if token == "" {
		return Credential{Kind: KindNone}
	}
	sum := sha256.Sum256([]byte(token))
	return Credential{Token: token, Hash: hex.EncodeToString(sum[:]), Kind: KindOf(token)}
}

// KindOf classifies a token by prefix.
func KindOf(token string) string {
	switch {
	case strings.HasPrefix(token, "ghp_"):
		return KindClassicPAT
	case strings.HasPrefix(token, "github_pat_"):
		return KindFineGrained
	case strings.HasPrefix(token, "gho_"):
		return KindOAuth
	case strings.HasPrefix(token, "ghu_"):
		return KindUserToServer
	case strings.HasPrefix(token, "ghs_"):
		return KindInstallation
	case strings.HasPrefix(token, "eyJ") && strings.Count(token, ".") == 2:
		return KindAppJWT
	default:
		return KindOther
	}
}

// AppIDFromJWT returns the "iss" claim (the GitHub App ID or client ID) of an
// app JWT without verifying it. Verification is GitHub's job; this is only a label.
func AppIDFromJWT(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Iss any `json:"iss"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Iss == nil {
		return ""
	}
	switch v := claims.Iss.(type) {
	case string:
		return v
	case float64:
		return jsonNumber(v) // integer app IDs marshal without a decimal point
	default:
		return ""
	}
}

func jsonNumber(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}

// ShortHash is the fingerprint prefix shown in dashboards.
func ShortHash(hash string) string {
	if len(hash) < 12 {
		return hash
	}
	return hash[:12]
}
