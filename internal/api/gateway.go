package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/OpenLinker-ai/openlinker-registry/internal/auth"
	"github.com/OpenLinker-ai/openlinker-registry/internal/model"
	"github.com/OpenLinker-ai/openlinker-registry/internal/registry"
)

func (s *Server) gatewayRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /v1/admin/gateways", s.admin(s.listGateways))
	m.HandleFunc("POST /v1/admin/gateways", s.admin(s.createGateway))
	m.HandleFunc("POST /v1/admin/gateways/{consumer}/keys", s.admin(s.addGatewayKey))
	m.HandleFunc("POST /v1/admin/gateways/{consumer}/keys/{kid}/revoke", s.admin(s.revokeGatewayKey))
}

func assertionError() *registry.Error {
	return &registry.Error{Status: http.StatusUnauthorized, Code: "assertion_invalid", Msg: "invalid user assertion"}
}

func (s *Server) assertionAudience() string {
	if s.AssertionAudience != "" {
		return s.AssertionAudience
	}
	return s.Svc.Cfg.PublicURL
}

func (s *Server) withAssertion(w http.ResponseWriter, r *http.Request, p auth.Principal) (*http.Request, bool) {
	raw := r.Header.Get(auth.AssertionHeader)
	if raw == "" {
		return r, true
	}
	if p.Gateway == nil || p.Admin {
		s.reject(w, r, assertionError())
		return r, false
	}
	budget := "assertion:" + p.Gateway.Consumer
	if over, wait := s.AuthFailLimit.Exhausted(budget); over {
		s.reject(w, r, tooManyRequests(wait))
		return r, false
	}
	invalid := func() (*http.Request, bool) {
		s.AuthFailLimit.Allow(budget)
		s.reject(w, r, assertionError())
		return r, false
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return invalid()
	}
	for _, values := range query {
		if len(values) != 1 {
			return invalid()
		}
	}
	var body []byte
	if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Body != nil {
		limit := s.MaxUploadBytes
		if limit < maxJSONBody {
			limit = maxJSONBody
		}
		data, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
		if err != nil {
			s.AuthFailLimit.Allow(budget)
			s.reject(w, r, &registry.Error{Status: http.StatusBadRequest, Code: "invalid_request", Msg: "request body could not be read"})
			return r, false
		}
		if int64(len(data)) > limit {
			s.AuthFailLimit.Allow(budget)
			s.reject(w, r, &registry.Error{Status: http.StatusRequestEntityTooLarge, Code: "too_large", Msg: "request body too large"})
			return r, false
		}
		body = data
		r.Body = io.NopCloser(bytes.NewReader(data))
	}
	act := auth.ActWrite
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		act = auth.ActRead
	}
	if strings.HasPrefix(strings.TrimPrefix(r.URL.Path, s.BasePath), "/v1/projections/") {
		act = auth.ActProject
	}
	verified, err := s.Auth.VerifyAssertion(r.Context(), p, raw, auth.AssertionRequest{Method: r.Method,
		Target: auth.CanonicalTarget(r.URL.EscapedPath(), query), Body: body, Audience: s.assertionAudience(), Act: act})
	if err != nil {
		if !errors.Is(err, auth.ErrInvalidAssertion) {
			s.Log.Error("assertion backend error", "err", err.Error())
			s.reject(w, r, &registry.Error{Status: http.StatusServiceUnavailable, Code: "unavailable", Msg: "authentication unavailable"})
			return r, false
		}
		return invalid()
	}
	return r.WithContext(context.WithValue(r.Context(), ctxKey{}, verified)), true
}

type gatewayRequest struct {
	Consumer   string   `json:"consumer"`
	TokenID    string   `json:"token_id"`
	Identities []string `json:"identities"`
}

func (s *Server) createGateway(w http.ResponseWriter, r *http.Request) {
	var req gatewayRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	if !auth.ValidConsumer(req.Consumer) {
		writeError(w, &registry.Error{Status: 422, Code: "consumer_invalid", Msg: "consumer must be a lowercase slug of at most 40 characters"})
		return
	}
	if len(req.Identities) == 0 || len(req.Identities) > 2 {
		writeError(w, &registry.Error{Status: 422, Code: "identities_invalid", Msg: "identities must list openlinker, github or both"})
		return
	}
	for _, id := range req.Identities {
		if id != model.IdentityOpenLinker && id != model.IdentityGitHub {
			writeError(w, &registry.Error{Status: 422, Code: "identities_invalid", Msg: "identities must list openlinker, github or both"})
			return
		}
	}
	tok, err := s.Svc.Store.GetToken(r.Context(), req.TokenID)
	if errors.Is(err, model.ErrNotFound) {
		writeError(w, &registry.Error{Status: 422, Code: "token_not_found", Msg: "unknown token"})
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if tok.RevokedAt != nil || slices.Contains(tok.Scopes, auth.ScopeAdmin) || !slices.Contains(tok.Scopes, auth.ScopeRead) {
		writeError(w, &registry.Error{Status: 422, Code: "token_invalid", Msg: "a gateway token must be active, carry skills:read and not be an admin token"})
		return
	}
	ids := slices.Clone(req.Identities)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	err = s.Svc.Store.CreateGateway(r.Context(), model.Gateway{Consumer: req.Consumer, TokenID: req.TokenID, Identities: ids},
		model.AuditEvent{Actor: principal(r).Subject, Action: "gateway.create", Target: req.Consumer,
			Details: map[string]any{"token": "token:" + req.TokenID, "identities": ids}})
	if errors.Is(err, model.ErrNotFound) {
		writeError(w, &registry.Error{Status: 422, Code: "token_not_found", Msg: "unknown token"})
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"consumer": req.Consumer, "token_id": req.TokenID, "identities": ids})
}

func (s *Server) listGateways(w http.ResponseWriter, r *http.Request) {
	gs, ks, err := s.Svc.Store.ListGateways(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(gs))
	for _, g := range gs {
		keys := []map[string]any{}
		for _, k := range ks {
			if k.Consumer == g.Consumer {
				keys = append(keys, map[string]any{"kid": k.KID, "public_key": base64.StdEncoding.EncodeToString(k.PublicKey),
					"created_at": k.CreatedAt, "revoked_at": k.RevokedAt})
			}
		}
		out = append(out, map[string]any{"consumer": g.Consumer, "token_id": g.TokenID, "identities": g.Identities,
			"projection": g.Projection, "created_at": g.CreatedAt, "keys": keys})
	}
	writeJSON(w, http.StatusOK, map[string]any{"gateways": out})
}

type gatewayKeyRequest struct {
	KID       string `json:"kid"`
	PublicKey string `json:"public_key"`
}

func (s *Server) addGatewayKey(w http.ResponseWriter, r *http.Request) {
	var req gatewayKeyRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	key, err := base64.StdEncoding.DecodeString(req.PublicKey)
	if !auth.ValidKID(req.KID) || err != nil || len(key) != ed25519.PublicKeySize {
		writeError(w, &registry.Error{Status: 422, Code: "key_invalid", Msg: "kid must be 1-64 of A-Z a-z 0-9 . _ - and public_key a base64 Ed25519 public key"})
		return
	}
	consumer := r.PathValue("consumer")
	err = s.Svc.Store.AddGatewayKey(r.Context(), model.GatewayKey{Consumer: consumer, KID: req.KID, PublicKey: key},
		model.AuditEvent{Actor: principal(r).Subject, Action: "gateway.key.add", Target: consumer, Details: map[string]any{"kid": req.KID}})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"consumer": consumer, "kid": req.KID})
}

func (s *Server) revokeGatewayKey(w http.ResponseWriter, r *http.Request) {
	consumer, kid := r.PathValue("consumer"), r.PathValue("kid")
	err := s.Svc.Store.RevokeGatewayKey(r.Context(), consumer, kid, time.Now().UTC(),
		model.AuditEvent{Actor: principal(r).Subject, Action: "gateway.key.revoke", Target: consumer, Details: map[string]any{"kid": kid}})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"consumer": consumer, "kid": kid, "revoked": true})
}
