package testkit_test

import (
	"testing"

	"github.com/wtwerner/heraldarr/internal/domain"
	"github.com/wtwerner/heraldarr/internal/testkit"
)

func TestMemStoreContract(t *testing.T) {
	testkit.StoreContract(t, func(*testing.T) domain.Store { return testkit.NewMemStore() }, nil)
}
