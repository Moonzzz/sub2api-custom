package audit

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"
)

func TestConfigFromViperEnvAndDefaults(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)
	cfg := ConfigFromViper()
	require.True(t, cfg.Enabled)
	require.Equal(t, "./data/audit", cfg.RootDir)
	require.Equal(t, DefaultSessionHeaders, cfg.SessionHeaders)
	require.Equal(t, 72*time.Hour, cfg.ArchiveAfter)
	require.Equal(t, "03:00", cfg.ArchiveAt)

	viper.AutomaticEnv()
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	t.Setenv("AUDIT_ENABLED", "false")
	t.Setenv("AUDIT_ROOT_DIR", "/srv/audit")
	t.Setenv("AUDIT_SESSION_HEADERS", "X-A,X-B")
	t.Setenv("AUDIT_FINALIZE_TIMEOUT", "5s")
	t.Setenv("AUDIT_ENCODER_LEVEL", "better")
	t.Setenv("AUDIT_ACTIVE_DIR", "/ssd/active")
	t.Setenv("AUDIT_ARCHIVE_DIR", "/hdd/archive")
	t.Setenv("AUDIT_ARCHIVE_AFTER", "48h")
	t.Setenv("AUDIT_ARCHIVE_AT", "04:30")
	t.Setenv("AUDIT_ARCHIVE_WINDOW_MB", "64")
	cfg = ConfigFromViper()
	require.False(t, cfg.Enabled)
	require.Equal(t, "/srv/audit", cfg.RootDir)
	require.Equal(t, []string{"X-A", "X-B"}, cfg.SessionHeaders)
	require.Equal(t, 5*time.Second, cfg.FinalizeTimeout)
	require.Equal(t, "better", cfg.EncoderLevel)
	require.Equal(t, "/ssd/active", cfg.ActiveDir)
	require.Equal(t, "/hdd/archive", cfg.ArchiveDir)
	require.Equal(t, 48*time.Hour, cfg.ArchiveAfter)
	require.Equal(t, "04:30", cfg.ArchiveAt)
	require.Equal(t, 64, cfg.ArchiveWindowMB)
}

func TestNewStoreDirs(t *testing.T) {
	root := t.TempDir()
	s, err := NewStore(Config{RootDir: root})
	require.NoError(t, err)
	require.Equal(t, filepath.Join(root, "active"), s.ActiveDir())
	require.Equal(t, filepath.Join(root, "archive"), s.ArchiveDir())
	_, err = NewStore(Config{ActiveDir: root, ArchiveDir: filepath.Join(root, "archive")})
	require.Error(t, err, "archive 不得位于 active 之内")
}
