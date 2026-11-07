package observability

import (
	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/require"
	"os"
	"testing"
)

// Target labels must not replace application dimensions under Prometheus's
// default honor_labels=false. In particular, planner and answer must survive.
func TestObservationScrapeLabelsPreserveLLMRole(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/observability/prometheus.yml")
	require.NoError(t, err)
	var cfg struct {
		Scrapes []struct {
			Job         string `yaml:"job_name"`
			HonorLabels bool   `yaml:"honor_labels"`
			Static      []struct {
				Labels map[string]string `yaml:"labels"`
			} `yaml:"static_configs"`
		} `yaml:"scrape_configs"`
	}
	require.NoError(t, yaml.Unmarshal(raw, &cfg))
	rt, _ := contractRuntime(t)
	observer, err := NewLLMObserver(rt)
	require.NoError(t, err)
	observer.duration.WithLabelValues("planner", "success").Observe(0.1)
	families, err := rt.Registry.Gather()
	require.NoError(t, err)
	found := false
	for _, scrape := range cfg.Scrapes {
		if scrape.Job != "video-support-agent-api" {
			continue
		}
		found = true
		for _, target := range scrape.Static {
			for _, metric := range families[0].Metric {
				for _, label := range metric.Label {
					if !scrape.HonorLabels {
						_, collision := target.Labels[label.GetName()]
						require.False(t, collision, "target label %q would replace an LLM metric dimension", label.GetName())
					}
				}
			}
		}
	}
	require.True(t, found, "API scrape target missing")
}
