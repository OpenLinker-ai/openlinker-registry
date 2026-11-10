// Package auth implements registry API tokens and caller principals.
//
// Token format: "olr_<id>_<secret>". The fixed "olr_" prefix lets a reverse
// proxy recognize registry tokens (design §7). Only sha256(secret) is stored;
// comparison is constant-time.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/OpenLinker-ai/openlinker-registry/internal/model"
	"github.com/OpenLinker-ai/openlinker-registry/internal/store"
)

// Token prefix and scopes.
const (
	Prefix      = "olr_"
	ScopeRead   = "skills:read"
	ScopeWrite  = "skills:write"
	ScopeAdmin  = "admin"
	adminSubjet = "admin:bootstrap"
)

// ValidScopes lists the scopes a token may carry.
var ValidScopes = []string{ScopeRead, ScopeWrite, ScopeAdmin}

// ErrInvalidToken is returned for any unusable credential. The reason is not
// distinguished to callers.
var ErrInvalidToken = errors.New("invalid token")

var idRe = regexp.MustCompile(`^[a-z0-9]{16,64}$`)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewToken returns a fresh plaintext token and its stored record fields.
func NewToken() (plaintext, id string, secretHash []byte, err error) {
	idBytes := make([]byte, 10)
	secret := make([]byte, 32)
	if _, err = rand.Read(idBytes); err != nil {
		return
	}
	if _, err = rand.Read(secret); err != nil {
		return
	}
	id = strings.ToLower(b32.EncodeToString(idBytes))
	s := base64.RawURLEncoding.EncodeToString(secret)
	h := sha256.Sum256([]byte(s))
	return Prefix + id + "_" + s, id, h[:], nil
}

// ParseToken splits a plaintext token into id and secret.
func ParseToken(tok string) (id, secret string, ok bool) {
	rest, ok := strings.CutPrefix(tok, Prefix)
	if !ok {
		return "", "", false
	}
	id, secret, ok = strings.Cut(rest, "_")
	if !ok || !idRe.MatchString(id) || len(secret) < 32 || len(secret) > 128 {
		return "", "", false
	}
	return id, secret, true
}

// Principal is the authenticated caller.
type Principal struct {
	Subject       string
	Authenticated bool
	Admin         bool
	Scopes        []string
	Roles         map[string]model.Role
}

// Anonymous is the unauthenticated principal.
var Anonymous = Principal{Subject: "anonymous"}

// HasScope reports whether the principal carries scope (admins carry all).
func (p Principal) HasScope(scope string) bool {
	return p.Admin || slices.Contains(p.Scopes, scope)
}

// HasRole reports whether the principal holds at least min in namespace.
func (p Principal) HasRole(namespace string, min model.Role) bool {
	if p.Admin {
		return true
	}
	return p.Roles[namespace].Rank() >= min.Rank()
}

// Viewer derives the visibility filter. A token without skills:read or
// skills:write sees only public content.
func (p Principal) Viewer() model.Viewer {
	v := model.Viewer{Admin: p.Admin}
	if !p.Authenticated || !(p.HasScope(ScopeRead) || p.HasScope(ScopeWrite)) {
		return v
	}
	v.Authenticated = true
	for ns := range p.Roles {
		v.Namespaces = append(v.Namespaces, ns)
	}
	slices.Sort(v.Namespaces)
	return v
}

// Authenticator resolves bearer tokens.
type Authenticator struct {
	Store     store.Store
	adminHash []byte
	Now       func() time.Time
}

// NewAuthenticator configures the optional bootstrap admin token. An empty
// adminToken disables bootstrap admin access.
func NewAuthenticator(st store.Store, adminToken string) *Authenticator {
	a := &Authenticator{Store: st, Now: time.Now}
	if adminToken != "" {
		h := sha256.Sum256([]byte(adminToken))
		a.adminHash = h[:]
	}
	return a
}

// Authenticate resolves a plaintext bearer token.
func (a *Authenticator) Authenticate(ctx context.Context, tok string) (Principal, error) {
	if tok == "" {
		return Principal{}, ErrInvalidToken
	}
	if a.adminHash != nil {
		h := sha256.Sum256([]byte(tok))
		if subtle.ConstantTimeCompare(h[:], a.adminHash) == 1 {
			return Principal{Subject: adminSubjet, Authenticated: true, Admin: true, Scopes: slices.Clone(ValidScopes)}, nil
		}
	}
	id, secret, ok := ParseToken(tok)
	if !ok {
		return Principal{}, ErrInvalidToken
	}
	t, err := a.Store.GetToken(ctx, id)
	if err != nil {
		if errors.Is(err, model.ErrNotFound) {
			// Burn comparable time for unknown ids.
			h := sha256.Sum256([]byte(secret))
			subtle.ConstantTimeCompare(h[:], make([]byte, 32))
			return Principal{}, ErrInvalidToken
		}
		return Principal{}, err
	}
	h := sha256.Sum256([]byte(secret))
	if subtle.ConstantTimeCompare(h[:], t.SecretHash) != 1 {
		return Principal{}, ErrInvalidToken
	}
	now := a.Now()
	if t.RevokedAt != nil || (t.ExpiresAt != nil && !now.Before(*t.ExpiresAt)) {
		return Principal{}, ErrInvalidToken
	}
	subject := "token:" + t.ID
	roles, err := a.Store.RolesForSubject(ctx, subject)
	if err != nil {
		return Principal{}, err
	}
	return Principal{Subject: subject, Authenticated: true, Admin: slices.Contains(t.Scopes, ScopeAdmin),
		Scopes: slices.Clone(t.Scopes), Roles: roles}, nil
}
