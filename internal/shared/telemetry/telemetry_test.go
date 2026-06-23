package telemetry

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestResolveServiceInstanceID(t *testing.T) {
	t.Parallel()

	t.Run("POD_NAME has priority", func(t *testing.T) {
		t.Parallel()
		got := resolveServiceInstanceID("pod-1", func() (string, error) {
			return "host-1", nil
		}, func() string { return "uuid-1" })
		assert.Equal(t, "pod-1", got)
	})

	t.Run("hostname fallback", func(t *testing.T) {
		t.Parallel()
		got := resolveServiceInstanceID("", func() (string, error) {
			return "host-1", nil
		}, func() string { return "uuid-1" })
		assert.Equal(t, "host-1", got)
	})

	t.Run("uuid fallback", func(t *testing.T) {
		t.Parallel()
		got := resolveServiceInstanceID("", func() (string, error) {
			return "", errors.New("hostname failed")
		}, func() string { return "uuid-1" })
		assert.Equal(t, "uuid-1", got)
	})
}
