package api_test

import (
	"testing"

	"github.com/OpenLinker-ai/openlinker-registry/internal/apitest"
	"github.com/OpenLinker-ai/openlinker-registry/internal/store"
)

func TestEndToEndMemoryStore(t *testing.T) {
	apitest.Run(t, func(*testing.T) store.Store { return store.NewMemory() })
}
