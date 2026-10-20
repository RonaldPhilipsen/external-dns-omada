package main

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestJoinShutdown(t *testing.T) {
	// Sentinel instances: errors.Is matches by identity, so the targets must
	// be the same values that flow into joinShutdown.
	errServe := errors.New("serve failed")
	errFlush := errors.New("collector down")
	t.Run("clean shutdown with flush failure stays clean", func(t *testing.T) {
		assert.NoError(t, joinShutdown(nil, errFlush))
	})
	t.Run("flush failure joins when a server already failed", func(t *testing.T) {
		err := joinShutdown([]error{errServe}, errFlush)
		require.Error(t, err)
		assert.ErrorIs(t, err, errServe)
		assert.ErrorIs(t, err, errFlush)
	})
	t.Run("clean shutdown with a non-nil empty error slice stays clean", func(t *testing.T) {
		assert.NoError(t, joinShutdown([]error{}, errFlush))
	})
	t.Run("no errors", func(t *testing.T) {
		assert.NoError(t, joinShutdown(nil, nil))
	})
}
