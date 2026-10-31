package steamcmd

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestParseInstallTimeout(t *testing.T) {
	cases := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"", 0, false},
		{"20m", 20 * time.Minute, false},
		{"1h30m", 90 * time.Minute, false},
		{"20 m", 0, true},
		{"20", 0, true},
		{"soon", 0, true},
		{"0s", 0, true},
		{"-5m", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := parseInstallTimeout(tc.in)
			if tc.wantErr {
				require.Error(t, err)
				require.Contains(t, err.Error(), "install_timeout")
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}
