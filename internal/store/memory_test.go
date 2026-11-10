package store_test

import (
	"testing"

	"github.com/OpenLinker-ai/openlinker-registry/internal/store"
	"github.com/OpenLinker-ai/openlinker-registry/internal/store/storetest"
)

func TestMemoryConformance(t *testing.T) {
	storetest.Run(t, func(*testing.T) store.Store { return store.NewMemory() })
}
