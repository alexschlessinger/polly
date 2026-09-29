package sessions_test

import (
	"testing"

	"github.com/alexschlessinger/pollytool/internal/sessiontest"
)

func TestStorageContract(t *testing.T) {
	for _, factory := range sessiontest.Factories() {
		t.Run(factory.Name, func(t *testing.T) { t.Parallel(); sessiontest.Run(t, factory.New) })
	}
}
