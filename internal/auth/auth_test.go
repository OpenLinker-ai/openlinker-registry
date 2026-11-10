package auth

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/OpenLinker-ai/openlinker-registry/internal/model"
	"github.com/OpenLinker-ai/openlinker-registry/internal/store"
)

func TestTokens(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	st.CreateNamespace(ctx, model.Namespace{Name: "com.example", Kind: "domain"})
	plain, id, hash, err := NewToken()
	if err != nil || !strings.HasPrefix(plain, Prefix) {
		t.Fatal(err)
	}
	if gotID, _, ok := ParseToken(plain); !ok || gotID != id {
		t.Fatal("parse")
	}
	exp := time.Now().Add(time.Hour)
	st.CreateToken(ctx, model.Token{ID: id, SecretHash: hash, Name: "t", Scopes: []string{ScopeWrite}, ExpiresAt: &exp},
		[]model.RoleBinding{{Namespace: "com.example", Role: model.RolePublisher}})
	a := NewAuthenticator(st, strings.Repeat("A", 40))
	p, err := a.Authenticate(ctx, plain)
	if err != nil || p.Subject != "token:"+id || !p.HasRole("com.example", model.RolePublisher) || p.HasRole("com.example", model.RoleOwner) || p.Admin {
		t.Fatalf("%+v %v", p, err)
	}
	if v := p.Viewer(); !v.Authenticated || len(v.Namespaces) != 1 {
		t.Fatalf("viewer %+v", v)
	}
	if _, err := a.Authenticate(ctx, plain[:len(plain)-1]+"x"); err != ErrInvalidToken {
		t.Fatal("wrong secret accepted")
	}
	if p, err := a.Authenticate(ctx, strings.Repeat("A", 40)); err != nil || !p.Admin {
		t.Fatal("bootstrap admin")
	}
	a.Now = func() time.Time { return exp.Add(time.Second) }
	if _, err := a.Authenticate(ctx, plain); err != ErrInvalidToken {
		t.Fatal("expired token accepted")
	}
	if _, err := NewAuthenticator(st, "").Authenticate(ctx, ""); err != ErrInvalidToken {
		t.Fatal("empty admin token must not match empty input")
	}
}
