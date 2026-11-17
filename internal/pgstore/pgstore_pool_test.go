package pgstore_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/OpenLinker-ai/openlinker-registry/internal/pgstore"
)

// A mirror lock keeps one connection while its holder works on another, so
// a pool of one connection would wait on itself. It is refused at startup.
func TestPoolNeedsTwoConnections(t *testing.T) {
	dsn := os.Getenv(DSNEnv)
	if dsn == "" {
		t.Skip(DSNEnv + " is not set; skipping Postgres integration tests")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 1
	if st, err := pgstore.OpenConfig(context.Background(), cfg); err == nil {
		st.Close()
		t.Fatal("a pool of one connection was accepted")
	} else if !strings.Contains(err.Error(), "pool_max_conns") {
		t.Fatalf("error does not name the setting: %v", err)
	}
	cfg.MaxConns = 2
	st, err := pgstore.OpenConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// With two connections a lock holder can still use the store.
	release, err := st.LockMirror(context.Background(), "skill:pool/test")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := st.Ping(context.Background()); err != nil {
		t.Fatalf("store unusable while a mirror lock is held: %v", err)
	}
}
