package main

import (
	"net/http"
	"sync"

	"contrib.go.opencensus.io/exporter/prometheus"
	prom "github.com/prometheus/client_golang/prometheus"
	promModel "github.com/prometheus/client_model/go"
	log "github.com/sirupsen/logrus"
	"go.opencensus.io/plugin/ochttp"
	"go.opencensus.io/stats/view"

	"github.com/mulbc/gosbench/common"
)

const (
	namespace = "gosbench"
)

var (
	registerOnce  sync.Once
	promRegistry  = prom.NewRegistry()
	promTestStart = prom.NewGaugeVec(
		prom.GaugeOpts{
			Name:      "test_start",
			Namespace: namespace,
			Help:      "Determines the start time of a job for Grafana annotations",
		}, []string{"testName"})
	promTestEnd = prom.NewGaugeVec(
		prom.GaugeOpts{
			Name:      "test_end",
			Namespace: namespace,
			Help:      "Determines the end time of a job for Grafana annotations",
		}, []string{"testName"})
	promFinishedOps = prom.NewCounterVec(
		prom.CounterOpts{
			Name:      "finished_ops",
			Namespace: namespace,
			Help:      "Finished S3 operations",
		}, []string{"testName", "method"})
	promFailedOps = prom.NewCounterVec(
		prom.CounterOpts{
			Name:      "failed_ops",
			Namespace: namespace,
			Help:      "Failed S3 operations",
		}, []string{"testName", "method"})
	promLatency = prom.NewHistogramVec(
		prom.HistogramOpts{
			Name:      "ops_latency",
			Namespace: namespace,
			Help:      "Histogram latency(ms) of S3 operations",
			Buckets:   prom.ExponentialBuckets(0.5, 2, 20),
		}, []string{"testName", "method"})
	promUploadedBytes = prom.NewCounterVec(
		prom.CounterOpts{
			Name:      "uploaded_bytes",
			Namespace: namespace,
			Help:      "Uploaded bytes to S3 store",
		}, []string{"testName", "method"})
	promDownloadedBytes = prom.NewCounterVec(
		prom.CounterOpts{
			Name:      "downloaded_bytes",
			Namespace: namespace,
			Help:      "Downloaded bytes from S3 store",
		}, []string{"testName", "method"})
	promGenBytesLatency = prom.NewHistogramVec(
		prom.HistogramOpts{
			Name:      "gen_bytes_latency",
			Namespace: namespace,
			Help:      "Histogram latency(ms) of generating random bytes",
			Buckets:   prom.ExponentialBuckets(0.001, 2, 18),
		}, []string{"testName"})
	promGenBytesSize = prom.NewGaugeVec(
		prom.GaugeOpts{
			Name:      "gen_bytes_size",
			Namespace: namespace,
			Help:      "Generated random bytes size of put object",
		}, []string{"testName"})
)

func newHandler() http.Handler {
	var err error
	registerOnce.Do(func() {
		if err = promRegistry.Register(promTestStart); err != nil {
			log.WithError(err).Error("Issues when adding test_start gauge to Prometheus registry")
		}
		if err = promRegistry.Register(promTestEnd); err != nil {
			log.WithError(err).Error("Issues when adding test_end gauge to Prometheus registry")
		}
		if err = promRegistry.Register(promFinishedOps); err != nil {
			log.WithError(err).Error("Issues when adding finished_ops counter to Prometheus registry")
		}
		if err = promRegistry.Register(promFailedOps); err != nil {
			log.WithError(err).Error("Issues when adding failed_ops counter to Prometheus registry")
		}
		if err = promRegistry.Register(promLatency); err != nil {
			log.WithError(err).Error("Issues when adding ops_latency histogram to Prometheus registry")
		}
		if err = promRegistry.Register(promGenBytesLatency); err != nil {
			log.WithError(err).Error("Issues when adding gen_bytes_latency histogram to Prometheus registry")
		}
		if err = promRegistry.Register(promGenBytesSize); err != nil {
			log.WithError(err).Error("Issues when adding gen_bytes_size gauge to Prometheus registry")
		}
		if err = promRegistry.Register(promUploadedBytes); err != nil {
			log.WithError(err).Error("Issues when adding uploaded_bytes counter to Prometheus registry")
		}
		if err = promRegistry.Register(promDownloadedBytes); err != nil {
			log.WithError(err).Error("Issues when adding downloaded_bytes counter to Prometheus registry")
		}
	})

	// promDownloadedBytes.WithLabelValues("test", "GET").Add(float64(1111)) // test
	pe, err := prometheus.NewExporter(prometheus.Options{
		Namespace: namespace,
		ConstLabels: map[string]string{
			"version": "0.0.1",
		},
		Registry: promRegistry,
	})
	if err != nil {
		log.WithError(err).Fatalf("Failed to create the Prometheus exporter")
	}

	if err := view.Register([]*view.View{
		ochttp.ClientSentBytesDistribution,
		ochttp.ClientReceivedBytesDistribution,
		ochttp.ClientRoundtripLatencyDistribution,
		ochttp.ClientCompletedCount,
	}...); err != nil {
		log.WithError(err).Fatalf("Failed to register HTTP client views:")
	}
	view.RegisterExporter(pe)

	return pe
}

func (w *Worker) getCurrentPromValues() common.BenchmarkResult {
	testName := w.config.Test.Name
	benchResult := common.BenchmarkResult{
		TestName: testName,
	}
	result, err := promRegistry.Gather()
	if err != nil {
		log.WithError(err).Error("ERROR during PROM VALUE gathering")
	}
	resultmap := map[string][]*promModel.Metric{}
	for _, metric := range result {
		resultmap[*metric.Name] = metric.Metric
	}
	benchResult.Operations = sumCounterForTest(resultmap["gosbench_finished_ops"], testName)
	benchResult.Bytes = sumCounterForTest(resultmap["gosbench_uploaded_bytes"], testName) + sumCounterForTest(resultmap["gosbench_downloaded_bytes"], testName)
	benchResult.LatencyAvg = averageHistogramForTest(resultmap["gosbench_ops_latency"], testName)
	benchResult.GenBytesLatencyAvg = averageHistogramForTest(resultmap["gosbench_gen_bytes_latency"], testName)

	return benchResult
}

func sumCounterForTest(metrics []*promModel.Metric, testName string) float64 {
	sum := float64(0)
	for _, metric := range metrics {
		for _, label := range metric.Label {
			if *label.Name == "testName" && *label.Value == testName {
				sum += *metric.Counter.Value
			}
		}
	}
	return sum
}

func averageHistogramForTest(metrics []*promModel.Metric, testName string) float64 {
	sum := float64(0)
	count := float64(0)
	for _, metric := range metrics {
		for _, label := range metric.Label {
			if *label.Name == "testName" && *label.Value == testName {
				sum += *metric.Histogram.SampleSum
				count += float64(*metric.Histogram.SampleCount)
			}
		}
	}
	if count == 0 {
		return -1
	}
	return sum / count
}
