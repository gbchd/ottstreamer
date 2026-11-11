package streammem_test

import (
	"testing"

	"ottstreamer/internal/stream/streammem"
	"ottstreamer/internal/stream/streamtest"
)

func TestRepository(t *testing.T) {
	streamtest.RunRepositoryContract(t, func() streamtest.Store { return streammem.NewRepository() })
}
