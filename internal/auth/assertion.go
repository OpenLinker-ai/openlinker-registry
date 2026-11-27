package auth

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/OpenLinker-ai/openlinker-registry/internal/model"
)

const (
	AssertionHeader   = "OpenLinker-User-Assertion"
	AssertionType     = "openlinker-user-assertion+jwt"
	ActRead           = "read"
	ActImport         = "import"
	ActWrite          = "write"
	ActProject        = "project"
	MaxAssertionBytes = 8192
	maxAssertionLife  = 60 * time.Second
	assertionSkew     = 30 * time.Second
)

var ErrInvalidAssertion = errors.New("invalid assertion")

var (
	uuidRe     = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	githubIDRe = regexp.MustCompile(`^[1-9][0-9]{0,18}$`)
	jtiRe      = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)
	kidRe      = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
)

type User struct {
	Consumer string
	Sub      string
	GitHubID string
	Act      []string
	Roles    map[string]model.Role
}

func (u *User) Allows(act string) bool {
	return u != nil && slices.Contains(u.Act, act)
}

func OpenLinkerSubject(consumer, sub string) string { return "openlinker:" + consumer + ":" + sub }

func GitHubSubject(id string) string { return "github:" + id }

func ValidUserSubject(subject string) bool {
	if rest, ok := strings.CutPrefix(subject, "openlinker:"); ok {
		consumer, sub, ok := strings.Cut(rest, ":")
		return ok && ValidConsumer(consumer) && uuidRe.MatchString(sub)
	}
	if id, ok := strings.CutPrefix(subject, "github:"); ok {
		return githubIDRe.MatchString(id)
	}
	return false
}

var consumerRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

func ValidConsumer(name string) bool { return len(name) <= 40 && consumerRe.MatchString(name) }

func ValidKID(kid string) bool { return kidRe.MatchString(kid) }

func CanonicalTarget(escapedPath string, query url.Values) string {
	type kv struct{ k, v string }
	var pairs []kv
	for k, vs := range query {
		for _, v := range vs {
			pairs = append(pairs, kv{k, v})
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].k != pairs[j].k {
			return pairs[i].k < pairs[j].k
		}
		return pairs[i].v < pairs[j].v
	})
	parts := make([]string, 0, len(pairs))
	for _, p := range pairs {
		parts = append(parts, url.QueryEscape(p.k)+"="+url.QueryEscape(p.v))
	}
	if len(parts) == 0 {
		return escapedPath
	}
	return escapedPath + "?" + strings.Join(parts, "&")
}

type assertionHeader struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
	Kid string `json:"kid"`
}

type assertionClaims struct {
	Aud string   `json:"aud"`
	Cid string   `json:"cid"`
	Sub string   `json:"sub"`
	GH  string   `json:"gh"`
	Act []string `json:"act"`
	Req struct {
		M  string `json:"m"`
		T  string `json:"t"`
		BH string `json:"bh"`
	} `json:"req"`
	Iat int64  `json:"iat"`
	Exp int64  `json:"exp"`
	Jti string `json:"jti"`
}

type AssertionRequest struct {
	Method   string
	Target   string
	Body     []byte
	Audience string
	Act      string
}

func decodeSegment(seg string, out any) error {
	raw, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(out)
}

func (a *Authenticator) VerifyAssertion(ctx context.Context, p Principal, raw string, req AssertionRequest) (Principal, error) {
	if p.Gateway == nil || p.Admin || len(raw) > MaxAssertionBytes || req.Audience == "" {
		return Principal{}, ErrInvalidAssertion
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return Principal{}, ErrInvalidAssertion
	}
	var h assertionHeader
	var c assertionClaims
	if decodeSegment(parts[0], &h) != nil || decodeSegment(parts[1], &c) != nil {
		return Principal{}, ErrInvalidAssertion
	}
	if h.Alg != "EdDSA" || h.Typ != AssertionType || !ValidKID(h.Kid) {
		return Principal{}, ErrInvalidAssertion
	}
	key, err := a.Store.GetGatewayKey(ctx, p.Gateway.Consumer, h.Kid)
	if errors.Is(err, model.ErrNotFound) || (err == nil && key.RevokedAt != nil) {
		return Principal{}, ErrInvalidAssertion
	}
	if err != nil {
		return Principal{}, err
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(key.PublicKey) != ed25519.PublicKeySize || !ed25519.Verify(key.PublicKey, []byte(parts[0]+"."+parts[1]), sig) {
		return Principal{}, ErrInvalidAssertion
	}
	var githubID int64
	if c.GH != "" {
		id, err := strconv.ParseInt(c.GH, 10, 64)
		if err != nil || id <= 0 {
			return Principal{}, ErrInvalidAssertion
		}
		githubID = id
	}
	now := a.Now()
	iat, exp := time.Unix(c.Iat, 0), time.Unix(c.Exp, 0)
	switch {
	case c.Aud != req.Audience, c.Cid != p.Gateway.Consumer, !uuidRe.MatchString(c.Sub),
		c.GH != "" && !githubIDRe.MatchString(c.GH), !jtiRe.MatchString(c.Jti),
		!exp.After(iat), exp.Sub(iat) > maxAssertionLife,
		now.Before(iat.Add(-assertionSkew)), now.After(exp.Add(assertionSkew)),
		c.Req.M != req.Method, c.Req.T != req.Target:
		return Principal{}, ErrInvalidAssertion
	}
	bh := sha256.Sum256(req.Body)
	if c.Req.BH != hex.EncodeToString(bh[:]) || !slices.Contains(c.Act, req.Act) {
		return Principal{}, ErrInvalidAssertion
	}
	for _, act := range c.Act {
		if act != ActRead && act != ActImport && act != ActWrite && act != ActProject {
			return Principal{}, ErrInvalidAssertion
		}
	}
	ok, err := a.Store.ClaimAssertion(ctx, p.Gateway.Consumer, c.Jti, exp.Add(assertionSkew+time.Minute), now)
	if err != nil {
		return Principal{}, err
	}
	if !ok {
		return Principal{}, ErrInvalidAssertion
	}
	u := &User{Consumer: p.Gateway.Consumer, Sub: c.Sub, Act: slices.Clone(c.Act), Roles: map[string]model.Role{}}
	merge := func(roles map[string]model.Role) {
		for ns, role := range roles {
			if role.Rank() > u.Roles[ns].Rank() {
				u.Roles[ns] = role
			}
		}
	}
	if slices.Contains(p.Gateway.Identities, model.IdentityOpenLinker) {
		roles, err := a.Store.RolesForSubject(ctx, OpenLinkerSubject(p.Gateway.Consumer, c.Sub))
		if err != nil {
			return Principal{}, err
		}
		merge(roles)
	}
	if c.GH != "" && slices.Contains(p.Gateway.Identities, model.IdentityGitHub) {
		u.GitHubID = c.GH
		roles, err := a.Store.RolesForSubject(ctx, GitHubSubject(c.GH))
		if err != nil {
			return Principal{}, err
		}
		merge(roles)
		ns, err := a.Store.NamespaceForGitHubID(ctx, githubID)
		switch {
		case err == nil:
			merge(map[string]model.Role{ns.Name: model.RoleOwner})
		case !errors.Is(err, model.ErrNotFound):
			return Principal{}, err
		}
	}
	p.User = u
	p.Subject = "gateway:" + p.Gateway.Consumer + ":" + c.Sub
	return p, nil
}
