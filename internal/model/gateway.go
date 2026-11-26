package model

import "time"

const (
	IdentityOpenLinker = "openlinker"
	IdentityGitHub     = "github"
)

type Gateway struct {
	Consumer   string
	TokenID    string
	Identities []string
	CreatedAt  time.Time
}

type GatewayKey struct {
	Consumer  string
	KID       string
	PublicKey []byte
	CreatedAt time.Time
	RevokedAt *time.Time
}
