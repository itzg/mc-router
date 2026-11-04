package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestLogger() (*logrus.Logger, *bytes.Buffer) {
	var out bytes.Buffer
	logger := logrus.New()
	logger.SetOutput(&out)
	return logger, &out
}

func TestAddLogFile(t *testing.T) {
	t.Run("writes to the file and the existing output", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "mc-router.log")
		logger, out := newTestLogger()

		file, err := addLogFile(logger, path)
		require.NoError(t, err)
		logger.Info("client connected")
		require.NoError(t, file.Close())

		written, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Contains(t, string(written), "client connected")
		assert.Contains(t, out.String(), "client connected")
	})

	t.Run("appends to an existing file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "mc-router.log")
		require.NoError(t, os.WriteFile(path, []byte("earlier line\n"), 0o644))
		logger, _ := newTestLogger()

		file, err := addLogFile(logger, path)
		require.NoError(t, err)
		logger.Info("later line")
		require.NoError(t, file.Close())

		written, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Contains(t, string(written), "earlier line")
		assert.Contains(t, string(written), "later line")
	})

	t.Run("keeps logging to the existing output when the file can't be opened", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "missing", "mc-router.log")
		logger, out := newTestLogger()

		_, err := addLogFile(logger, path)
		require.Error(t, err)
		logger.Info("still here")

		assert.Contains(t, out.String(), "still here")
	})
}
