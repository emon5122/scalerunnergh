package app

import (
	"testing"
	"time"

	"github.com/emon5122/scalerunnergh/internal/config"
)

func TestPullIntervalsOnePerImageShortestWins(t *testing.T) {
	cfg := &config.Config{ScaleSets: []config.ScaleSetConfig{
		{Name: "a", Backend: config.BackendDocker, Image: "ghcr.io/acme/ci:main", Pull: "interval:1h"},
		{Name: "b", Backend: config.BackendDocker, Image: "ghcr.io/acme/ci:main", Pull: "interval:30m"},
		{Name: "c", Backend: config.BackendDocker, Image: "ghcr.io/acme/other:1", Pull: "always"},
		{Name: "d", Backend: config.BackendDocker, Image: "ghcr.io/acme/third:1"},
		{Name: "e", Backend: config.BackendNative},
	}}
	got := pullIntervals(cfg)
	if len(got) != 1 || got["ghcr.io/acme/ci:main"] != 30*time.Minute {
		t.Fatalf("pull intervals: %v", got)
	}
}
