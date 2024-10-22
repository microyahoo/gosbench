package main

import (
	"fmt"
	"io"
	"os"
	"time"

	"github.com/go-echarts/go-echarts/v2/charts"
	"github.com/go-echarts/go-echarts/v2/components"
	"github.com/go-echarts/go-echarts/v2/opts"
	"github.com/gocarina/gocsv"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

// t.AppendHeader(table.Row{"type", "object-size(KB)", "workers", "parallel-clients", "avg-bandwidth(MB/s)", "ops", "avg-latency(ms)",
//
//		"gen-bytes-avg-latency(ms)", "io-copy-avg-latency(ms)", "successful-ops", "failed-ops", "duration(s)", "total-mbytes", "name",
//	})
//
//	for _, r := range results {
//		t.AppendRow(table.Row{
//			r.Type.String(),
//			r.ObjectSize / 1024,
//			r.Workers,
//			r.ParallelClients,
//			fmt.Sprintf("%.1f", r.BandwidthAvg/1024/1024),
//			fmt.Sprintf("%.2f", r.BandwidthAvg/float64(r.ObjectSize)),
//			fmt.Sprintf("%.2f", r.LatencyAvg),
//			fmt.Sprintf("%.2f", r.GenBytesLatencyAvg),
//			fmt.Sprintf("%.2f", r.IOCopyLatencyAvg),
//			r.SuccessfulOperations,
//			r.FailedOperations,
//			r.Duration.Round(time.Second),
//			fmt.Sprintf("%.0f", r.Bytes/1024/1024),
//			r.TestName,
//		})
//	}
type Result struct {
	Type                 string   `csv:"type"`
	ObjectKBSize         uint64   `csv:"object-size(KB)"`
	Workers              uint64   `csv:"workers"`
	ParallelClients      uint64   `csv:"parallel-clients"`
	AvgMBBandwidth       float64  `csv:"avg-bandwidth(MB/s)"`
	Ops                  float64  `csv:"ops"`
	AvgLatencyMs         float64  `csv:"avg-latency(ms)"`
	GenBytesAvgLatencyMs float64  `csv:"gen-bytes-avg-latency(ms)"`
	IOCopyAvgLatencyMs   float64  `csv:"io-copy-avg-latency(ms)"`
	SuccessfulOps        float64  `csv:"successful-ops"`
	FailedOps            float64  `csv:"failed-ops"`
	Duration             Duration `csv:"duration"` // NOTE: remove the unit
	TotalMBytes          float64  `csv:"total-mbytes"`
	Name                 string   `csv:"name"`
}

type Duration struct {
	time.Duration
}

// Convert the internal duration as CSV string
func (d *Duration) MarshalCSV() (string, error) {
	return d.String(), nil
}

// Convert the CSV string as internal duration
func (d *Duration) UnmarshalCSV(csv string) (err error) {
	d.Duration, err = time.ParseDuration(csv)
	return err
}

var (
	first, second string
)

func main() {
	rootCmd := newCommand()
	cobra.CheckErr(rootCmd.Execute())
}

func newCommand() *cobra.Command {
	cmds := &cobra.Command{
		Use: "compare",
		Run: func(cmd *cobra.Command, args []string) {
			run()
		},
	}
	cmds.Flags().SortFlags = false
	cmds.Flags().StringVar(&first, "first.file", "", "First file wants to compare")
	cmds.Flags().StringVar(&second, "second.file", "", "Second file wants to compare")

	return cmds
}

func run() {
	if first == "" || second == "" {
		panic("compare files should be specified")
	}
	var (
		firstMap  = make(map[string]map[uint64]map[uint64]map[uint64]*Result) // type -> object-size -> workers -> parallel-clients -> result
		secondMap = make(map[string]map[uint64]map[uint64]map[uint64]*Result)
	)
	firstFile, err := os.Open(first)
	if err != nil {
		panic(err)
	}
	defer firstFile.Close()
	firstResults := []*Result{}
	if err := gocsv.UnmarshalFile(firstFile, &firstResults); err != nil { // Load clients from file
		panic(err)
	}
	for _, r := range firstResults {
		osMap, ok := firstMap[r.Type]
		if !ok {
			osMap = make(map[uint64]map[uint64]map[uint64]*Result)
			firstMap[r.Type] = osMap
		}
		workerMap, ok := osMap[r.ObjectKBSize]
		if !ok {
			workerMap = make(map[uint64]map[uint64]*Result)
			osMap[r.ObjectKBSize] = workerMap
		}
		pcMap, ok := workerMap[r.Workers]
		if !ok {
			pcMap = make(map[uint64]*Result)
			workerMap[r.Workers] = pcMap
		}
		pcMap[r.ParallelClients] = r
	}
	log.Info("---------------first------------------------")
	for t, m1 := range firstMap {
		for os, m2 := range m1 {
			for w, m3 := range m2 {
				for pc, r := range m3 {
					log.Infof("%s %d %d %d %v", t, os, w, pc, r)
				}
			}
		}
	}

	secondFile, err := os.Open(second)
	if err != nil {
		panic(err)
	}
	defer secondFile.Close()
	secondResults := []*Result{}
	if err := gocsv.UnmarshalFile(secondFile, &secondResults); err != nil { // Load clients from file
		panic(err)
	}
	for _, r := range secondResults {
		osMap, ok := secondMap[r.Type]
		if !ok {
			osMap = make(map[uint64]map[uint64]map[uint64]*Result)
			secondMap[r.Type] = osMap
		}
		workerMap, ok := osMap[r.ObjectKBSize]
		if !ok {
			workerMap = make(map[uint64]map[uint64]*Result)
			osMap[r.ObjectKBSize] = workerMap
		}
		pcMap, ok := workerMap[r.Workers]
		if !ok {
			pcMap = make(map[uint64]*Result)
			workerMap[r.Workers] = pcMap
		}
		pcMap[r.ParallelClients] = r
	}
	log.Info("---------------second------------------------")
	for t, m1 := range secondMap {
		for os, m2 := range m1 {
			for w, m3 := range m2 {
				for pc, r := range m3 {
					log.Infof("%s %d %d %d %v", t, os, w, pc, r)
				}
			}
		}
	}
	bandwidthPage := components.NewPage()
	avgLatencyPage := components.NewPage()
	genBytesPage := components.NewPage()
	ioCopyPage := components.NewPage()
	durationPage := components.NewPage()
	successfulPage := components.NewPage()
	failedPage := components.NewPage()
	opsPage := components.NewPage()
	log.Info("---------------compare------------------------")
	for t, osMap1 := range firstMap {
		if t == "delete" {
			continue
		}
		osMap2, ok := secondMap[t]
		if !ok {
			continue
		}
		for os1, workerMap1 := range osMap1 {
			workerMap2, ok := osMap2[os1]
			if !ok {
				continue
			}
			for w1, pcMap1 := range workerMap1 {
				pcMap2, ok := workerMap2[w1]
				if !ok {
					continue
				}
				title := fmt.Sprintf("%s-%dKB-%dw", t, os1, w1)
				avgMBBandwidth1 := make([]float64, 0)
				avgLatency1 := make([]float64, 0)
				ioCopyLatency1 := make([]float64, 0)
				genBytesLatency1 := make([]float64, 0)
				duration1 := make([]float64, 0)
				successfulOps1 := make([]float64, 0)
				failedOps1 := make([]float64, 0)
				avgMBBandwidth2 := make([]float64, 0)
				avgLatency2 := make([]float64, 0)
				ioCopyLatency2 := make([]float64, 0)
				genBytesLatency2 := make([]float64, 0)
				duration2 := make([]float64, 0)
				successfulOps2 := make([]float64, 0)
				failedOps2 := make([]float64, 0)
				ops1 := make([]float64, 0)
				ops2 := make([]float64, 0)
				pcs := make([]uint64, 0)
				for pc1, r1 := range pcMap1 {
					r2, ok := pcMap2[pc1]
					if !ok {
						continue
					}
					pcs = append(pcs, pc1)
					avgMBBandwidth1 = append(avgMBBandwidth1, r1.AvgMBBandwidth)
					avgMBBandwidth2 = append(avgMBBandwidth2, r2.AvgMBBandwidth)
					avgLatency1 = append(avgLatency1, r1.AvgLatencyMs)
					avgLatency2 = append(avgLatency2, r2.AvgLatencyMs)
					ioCopyLatency1 = append(ioCopyLatency1, r1.IOCopyAvgLatencyMs)
					ioCopyLatency2 = append(ioCopyLatency2, r2.IOCopyAvgLatencyMs)
					genBytesLatency1 = append(genBytesLatency1, r1.GenBytesAvgLatencyMs)
					genBytesLatency2 = append(genBytesLatency2, r2.GenBytesAvgLatencyMs)
					duration1 = append(duration1, r1.Duration.Seconds())
					duration2 = append(duration2, r2.Duration.Seconds())
					successfulOps1 = append(successfulOps1, r1.SuccessfulOps)
					successfulOps2 = append(successfulOps2, r2.SuccessfulOps)
					failedOps1 = append(failedOps1, r1.FailedOps)
					failedOps2 = append(failedOps2, r2.FailedOps)
					ops1 = append(ops1, r1.Ops)
					ops2 = append(ops2, r2.Ops)
					log.Infof("%s %d %d %d %v, %v", t, os1, w1, pc1, r1, r2)
				}
				bandwidthBar := generateBar(title, "bandwidth(MB/s)", pcs, avgMBBandwidth1, avgMBBandwidth2)
				avgLatencyBar := generateBar(title, "avg-latency(ms)", pcs, avgLatency1, avgLatency2)
				ioCopyBar := generateBar(title, "iocopy(ms)", pcs, ioCopyLatency1, ioCopyLatency2)
				genBytesBar := generateBar(title, "gen-bytes(ms)", pcs, genBytesLatency1, genBytesLatency2)
				durationBar := generateBar(title, "duration(s)", pcs, duration1, duration2)
				successfulOpsBar := generateBar(title, "successful-ops", pcs, successfulOps1, successfulOps2)
				failedOpsBar := generateBar(title, "failed-ops", pcs, failedOps1, failedOps2)
				opsBar := generateBar(title, "ops", pcs, ops1, ops2)

				bandwidthPage.AddCharts(bandwidthBar)
				avgLatencyPage.AddCharts(avgLatencyBar)
				if t == "write" {
					genBytesPage.AddCharts(genBytesBar)
				}
				if t == "read" {
					ioCopyPage.AddCharts(ioCopyBar)
				}
				durationPage.AddCharts(durationBar)
				successfulPage.AddCharts(successfulOpsBar)
				failedPage.AddCharts(failedOpsBar)
				opsPage.AddCharts(opsBar)
			}
		}
	}
	b, err := os.Create("examples/html/bandwidth-bar.html")
	if err != nil {
		panic(err)
	}
	bandwidthPage.Render(io.MultiWriter(b))
	a, err := os.Create("examples/html/avg-latency-bar.html")
	if err != nil {
		panic(err)
	}
	avgLatencyPage.Render(io.MultiWriter(a))
	g, err := os.Create("examples/html/genbytes-avg-latency-bar.html")
	if err != nil {
		panic(err)
	}
	genBytesPage.Render(io.MultiWriter(g))
	i, err := os.Create("examples/html/iocopy-avg-latency-bar.html")
	if err != nil {
		panic(err)
	}
	ioCopyPage.Render(io.MultiWriter(i))
	d, err := os.Create("examples/html/duration-bar.html")
	if err != nil {
		panic(err)
	}
	durationPage.Render(io.MultiWriter(d))
	s, err := os.Create("examples/html/successful-ops-bar.html")
	if err != nil {
		panic(err)
	}
	successfulPage.Render(io.MultiWriter(s))
	f, err := os.Create("examples/html/failed-ops-bar.html")
	if err != nil {
		panic(err)
	}
	failedPage.Render(io.MultiWriter(f))
	o, err := os.Create("examples/html/ops-bar.html")
	if err != nil {
		panic(err)
	}
	opsPage.Render(io.MultiWriter(o))
}

func generateBar(title, index string, pcs []uint64, val1, val2 []float64) *charts.Bar {
	bar := charts.NewBar()
	bar.SetGlobalOptions(
		charts.WithTitleOpts(opts.Title{Title: fmt.Sprintf("%s-%s", title, index)}),
	)

	bar.SetXAxis(pcs).
		AddSeries("r1", generateBarItems(val1)).
		AddSeries("r2", generateBarItems(val2))
	return bar
}

func generateBarItems(vals []float64) []opts.BarData {
	items := make([]opts.BarData, 0)
	for i := 0; i < len(vals); i++ {
		items = append(items, opts.BarData{Value: vals[i]})
	}
	return items
}
