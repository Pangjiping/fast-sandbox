package testutil

import (
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
)

// HistogramSampleCount returns the sample count for one exact Prometheus
// histogram label set. It keeps metric assertions independent of optional
// prometheus/testutil vendoring.
func HistogramSampleCount(name string, labels map[string]string) (uint64, error) {
	return HistogramSampleCountFrom(prometheus.DefaultGatherer, name, labels)
}

func HistogramSampleCountFrom(gatherer prometheus.Gatherer, name string, labels map[string]string) (uint64, error) {
	families, err := gatherer.Gather()
	if err != nil {
		return 0, err
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.Metric {
			matches := len(metric.Label) == len(labels)
			for _, pair := range metric.Label {
				if labels[pair.GetName()] != pair.GetValue() {
					matches = false
					break
				}
			}
			if matches && metric.Histogram != nil {
				return metric.Histogram.GetSampleCount(), nil
			}
		}
	}
	return 0, fmt.Errorf("histogram %s with labels %v not found", name, labels)
}
