package audit

import (
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

	viper.AutomaticEnv()
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	t.Setenv("AUDIT_ENABLED", "false")
	t.Setenv("AUDIT_ROOT_DIR", "/srv/audit")
	t.Setenv("AUDIT_SESSION_HEADERS", "X-A,X-B")
	t.Setenv("AUDIT_FINALIZE_TIMEOUT", "5s")
	t.Setenv("AUDIT_ENCODER_LEVEL", "better")
	cfg = ConfigFromViper()
	require.False(t, cfg.Enabled)
	require.Equal(t, "/srv/audit", cfg.RootDir)
	require.Equal(t, []string{"X-A", "X-B"}, cfg.SessionHeaders)
	require.Equal(t, 5*time.Second, cfg.FinalizeTimeout)
	require.Equal(t, "better", cfg.EncoderLevel)
}
