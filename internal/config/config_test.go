package config

import (
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestStreamMaxConfig(t *testing.T) {
	for _, tc := range []struct {
		name string
		yaml string
		want time.Duration
	}{
		{"default", "", 15 * time.Minute},
		{"zero", "timeouts:\n  stream_max: 0s\n", 15 * time.Minute},
		{"positive", "timeouts:\n  stream_max: 2m\n", 2 * time.Minute},
		{"disabled", "timeouts:\n  stream_max: -1s\n", -time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			if err := yaml.Unmarshal([]byte(tc.yaml), cfg); err != nil {
				t.Fatal(err)
			}
			if err := cfg.Validate(); err != nil {
				t.Fatal(err)
			}
			body, err := yaml.Marshal(cfg.Timeouts)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]string
			if err := yaml.Unmarshal(body, &fields); err != nil {
				t.Fatal(err)
			}
			if got := fields["stream_max"]; got != tc.want.String() {
				t.Errorf("stream_max=%q want=%q", got, tc.want)
			}
		})
	}
}
