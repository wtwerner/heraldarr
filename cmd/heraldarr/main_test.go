package main

import (
	"slices"
	"testing"
)

func TestInterspersed(t *testing.T) {
	for _, tc := range []struct{ in, want []string }{
		{[]string{"-config", "c.yaml", "serve"}, []string{"-config", "c.yaml", "serve"}},
		{[]string{"serve", "-config", "c.yaml"}, []string{"-config", "c.yaml", "serve"}},
		{[]string{"preview", "radarr", "5", "-to", "private"}, []string{"-to", "private", "preview", "radarr", "5"}},
		{[]string{"preview", "sonarr", "1", "S02", "-config=c.yaml"}, []string{"-config=c.yaml", "preview", "sonarr", "1", "S02"}},
	} {
		if got := interspersed(tc.in); !slices.Equal(got, tc.want) {
			t.Errorf("interspersed(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
