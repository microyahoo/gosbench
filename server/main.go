package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/jedib0t/go-pretty/v6/text"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/mulbc/gosbench/common"
)

var (
	configFile   string
	serverPort   int
	readyWorkers chan *net.Conn
	debug, trace bool
)

type Server struct {
	config *common.TestConf
}

func main() {
	rootCmd := newCommand()
	cobra.CheckErr(rootCmd.Execute())
}

func newCommand() *cobra.Command {
	cmds := &cobra.Command{
		Use: "gosbench-server",
		Run: func(cmd *cobra.Command, args []string) {
			run()
		},
	}
	cmds.Flags().SortFlags = false

	viper.SetDefault("DEBUG", false)
	viper.SetDefault("TRACE", false)
	viper.SetDefault("SERVERPORT", 2000)

	viper.AutomaticEnv()
	viper.AllowEmptyEnv(true)

	cmds.Flags().StringVar(&configFile, "config.file", viper.GetString("CONFIGFILE"), "Config file describing test run")
	cmds.Flags().BoolVar(&debug, "debug", viper.GetBool("DEBUG"), "enable debug log output")
	cmds.Flags().BoolVar(&trace, "trace", viper.GetBool("TRACE"), "enable trace log output")
	cmds.Flags().IntVar(&serverPort, "server.port", viper.GetInt("SERVERPORT"), "Port on which the server will be available for clients. Default: 2000")

	return cmds
}

func run() {
	if configFile == "" {
		log.Fatal("--config.file is a mandatory parameter - please specify the config file")
	}
	if debug {
		log.SetLevel(log.DebugLevel)
	} else if trace {
		log.SetLevel(log.TraceLevel)
	} else {
		log.SetLevel(log.InfoLevel)
	}
	log.Debugf("viper settings=%+v", viper.AllSettings())
	log.Debugf("gosbench server configFile=%s, serverPort=%d", configFile, serverPort)

	config := common.LoadConfigFromFile(configFile)
	common.CheckConfig(config)

	readyWorkers = make(chan *net.Conn)
	defer close(readyWorkers)

	// Listen on TCP port 2000 on all available unicast and
	// anycast IP addresses of the local system.
	l, err := net.Listen("tcp", fmt.Sprintf(":%d", serverPort))
	if err != nil {
		log.WithError(err).Fatal("Could not open port!")
	}
	defer l.Close()
	log.Info("Ready to accept connections")
	server := &Server{
		config: config,
	}
	go server.scheduleTests()
	for {
		// Wait for a connection.
		conn, err := l.Accept()
		if err != nil {
			log.WithError(err).Fatal("Issue when waiting for connection of clients")
		}
		// Handle the connection in a new goroutine.
		// The loop then returns to accepting, so that
		// multiple connections may be served concurrently.
		go func(c *net.Conn) {
			log.Infof("%s connected to us ", (*c).RemoteAddr())
			decoder := json.NewDecoder(*c)
			var message string
			err := decoder.Decode(&message)
			if err != nil {
				log.WithField("message", message).WithError(err).Error("Could not decode message, closing connection")
				(*c).Close()
				return
			}
			if message == "ready for work" {
				log.Debug("We have a new worker!")
				readyWorkers <- c
				return
			}
		}(&conn)
		// Shut down the connection.
		// defer conn.Close()
	}
}

func (s *Server) scheduleTests() {
	var (
		clientGatewayColocation = true
		results                 []*common.BenchmarkResult
	)
	gc := s.config.GlobalConfig
	if gc != nil && !gc.ClientGatewayColocation {
		clientGatewayColocation = false
	}

	for testNumber, test := range s.config.Tests {
		doneChannel := make(chan bool, test.Workers)
		resultChannel := make(chan common.BenchmarkResult, test.Workers)
		continueWorkers := make(chan bool, test.Workers)

		for worker := 0; worker < test.Workers; worker++ {
			workerConfig := &common.WorkerConf{
				Test:                    test,
				S3Configs:               s.config.S3Configs,
				WorkerID:                fmt.Sprintf("w%d", worker),
				ID:                      worker,
				ClientGatewayColocation: clientGatewayColocation,
			}
			workerConnection := <-readyWorkers
			log.WithField("Worker", (*workerConnection).RemoteAddr()).Infof("We found worker %d / %d for test %d", worker+1, test.Workers, testNumber)
			go executeTestOnWorker(workerConnection, workerConfig, doneChannel, continueWorkers, resultChannel)
		}
		for worker := 0; worker < test.Workers; worker++ {
			// Will halt until all workers are done with preparations
			<-doneChannel
		}
		log.WithField("test", test.Name).Info("All workers have finished preparations - starting performance test")
		startTime := time.Now().UTC()
		for worker := 0; worker < test.Workers; worker++ {
			continueWorkers <- true
		}
		var benchResults []common.BenchmarkResult
		for worker := 0; worker < test.Workers; worker++ {
			// Will halt until all workers are done with their work
			<-doneChannel
			benchResults = append(benchResults, <-resultChannel)
		}
		log.WithField("test", test.Name).Info("All workers have finished the performance test - continuing with next test")
		stopTime := time.Now().UTC()
		log.WithField("test", test.Name).Infof("GRAFANA: ?from=%d&to=%d", startTime.UnixNano()/int64(1000000), stopTime.UnixNano()/int64(1000000))
		benchResult := sumBenchmarkResults(benchResults)
		benchResult.Duration = stopTime.Sub(startTime)
		benchResult.ParallelClients = float64(test.ParallelClients)
		benchResult.Workers = float64(test.Workers)
		benchResult.ObjectSize = (test.Objects.SizeMin + test.Objects.SizeMax) / 2 // TODO: not exact
		if test.ReadWeight > 0 {
			benchResult.Type |= common.Read
		}
		if test.WriteWeight > 0 {
			benchResult.Type |= common.Write
		}
		if test.DeleteWeight > 0 {
			benchResult.Type |= common.Delete
		}
		if test.ListWeight > 0 {
			benchResult.Type |= common.List
		}

		log.WithField("test", test.Name).
			WithField("Successful Operations", benchResult.SuccessfulOperations).
			WithField("Failed Operations", benchResult.FailedOperations).
			WithField("Total MBytes", benchResult.Bytes/1024/1024).
			WithField("Average BW in MByte/s", benchResult.BandwidthAvg/1024/1024).
			WithField("Average latency in ms", benchResult.LatencyAvg).
			WithField("Gen Bytes Average latency in ms", benchResult.GenBytesLatencyAvg).
			WithField("Workers", benchResult.Workers).
			WithField("Type", benchResult.Type).
			WithField("Object size (not exact)", common.ByteSize(benchResult.ObjectSize)). // TODO: not exact
			WithField("Parallel clients", benchResult.ParallelClients).
			WithField("Test runtime on server", benchResult.Duration).
			Infof("PERF RESULTS")
		writeResultToCSV(benchResult)
		results = append(results, &benchResult)
	}
	log.Info("All performance tests finished")
	s.generateResults(results)
	for {
		workerConnection := <-readyWorkers
		shutdownWorker(workerConnection)
	}
}

func executeTestOnWorker(conn *net.Conn, config *common.WorkerConf, doneChannel chan bool, continueWorkers chan bool, resultChannel chan common.BenchmarkResult) {
	encoder := json.NewEncoder(*conn)
	decoder := json.NewDecoder(*conn)
	_ = encoder.Encode(common.WorkerMessage{Message: "init", Config: config})

	var response common.WorkerMessage
	for {
		err := decoder.Decode(&response)
		if err != nil {
			log.WithField("worker", config.WorkerID).WithField("message", response).WithError(err).Error("Worker responded unusually - dropping")
			(*conn).Close()
			return
		}
		log.Infof("Response: %+v", response)
		switch response.Message {
		case "preparations done":
			doneChannel <- true
			<-continueWorkers
			_ = encoder.Encode(common.WorkerMessage{Message: "start work"})
		case "work done":
			doneChannel <- true
			resultChannel <- response.BenchResult
			(*conn).Close() // TODO: not close connection
			return
		}
	}
}

func shutdownWorker(conn *net.Conn) {
	encoder := json.NewEncoder(*conn)
	log.WithField("Worker", (*conn).RemoteAddr()).Info("Shutting down worker")
	_ = encoder.Encode(common.WorkerMessage{Message: "shutdown"})
}

func sumBenchmarkResults(results []common.BenchmarkResult) common.BenchmarkResult {
	sum := common.BenchmarkResult{}
	bandwidthAverages := float64(0)
	latencyAverages := float64(0)
	genBytesLatencyAverages := float64(0)
	ioCopyLatencyAverages := float64(0)
	for _, result := range results {
		sum.Bytes += result.Bytes
		sum.SuccessfulOperations += result.SuccessfulOperations
		sum.FailedOperations += result.FailedOperations
		latencyAverages += result.LatencyAvg
		genBytesLatencyAverages += result.GenBytesLatencyAvg
		ioCopyLatencyAverages += result.IOCopyLatencyAvg
		bandwidthAverages += result.BandwidthAvg
	}
	sum.LatencyAvg = latencyAverages / float64(len(results))
	sum.GenBytesLatencyAvg = genBytesLatencyAverages / float64(len(results))
	sum.IOCopyLatencyAvg = ioCopyLatencyAverages / float64(len(results))
	sum.TestName = results[0].TestName
	sum.BandwidthAvg = bandwidthAverages

	return sum
}

func writeResultToCSV(benchResult common.BenchmarkResult) {
	file, created, err := getCSVFileHandle()
	if err != nil {
		log.WithError(err).Error("Could not get a file handle for the CSV results")
		return
	}
	defer file.Close()

	csvwriter := csv.NewWriter(file)

	if created {
		err = csvwriter.Write([]string{
			"testName",
			"Successful Operations",
			"Failed Operations",
			"Total Bytes",
			"Average Bandwidth in Bytes/s",
			"Average Latency in ms",
			"Gen Bytes Average Latency in ms",
			"IO Copy Average Latency in ms",
			"Workers",
			"Parallel clients",
			"Test duration seen by server in seconds",
		})
		if err != nil {
			log.WithError(err).Error("Failed writing line to results csv")
			return
		}
	}

	err = csvwriter.Write([]string{
		benchResult.TestName,
		fmt.Sprintf("%.0f", benchResult.SuccessfulOperations),
		fmt.Sprintf("%.0f", benchResult.FailedOperations),
		fmt.Sprintf("%.0f", benchResult.Bytes),
		fmt.Sprintf("%f", benchResult.BandwidthAvg),
		fmt.Sprintf("%f", benchResult.LatencyAvg),
		fmt.Sprintf("%f", benchResult.GenBytesLatencyAvg),
		fmt.Sprintf("%f", benchResult.IOCopyLatencyAvg),
		fmt.Sprintf("%f", benchResult.Workers),
		fmt.Sprintf("%f", benchResult.ParallelClients),
		fmt.Sprintf("%f", benchResult.Duration.Seconds()),
	})
	if err != nil {
		log.WithError(err).Error("Failed writing line to results csv")
		return
	}

	csvwriter.Flush()

}

func getCSVFileHandle() (*os.File, bool, error) {
	file, err := os.OpenFile("gosbench_results.csv", os.O_APPEND|os.O_WRONLY, 0755)
	if err == nil {
		return file, false, nil
	}
	file, err = os.OpenFile("/tmp/gosbench_results.csv", os.O_APPEND|os.O_WRONLY, 0755)
	if err == nil {
		return file, false, nil
	}

	file, err = os.OpenFile("gosbench_results.csv", os.O_WRONLY|os.O_CREATE, 0755)
	if err == nil {
		return file, true, nil
	}
	file, err = os.OpenFile("/tmp/gosbench_results.csv", os.O_WRONLY|os.O_CREATE, 0755)
	if err == nil {
		return file, true, nil
	}

	return nil, false, errors.New("Could not find previous CSV for appending and could not write new CSV file to current dir and /tmp/ giving up")

}

func (s *Server) generateResults(results []*common.BenchmarkResult) {
	t := table.NewWriter()
	var format = "csv"
	if s.config.ReportConfig != nil {
		format = s.config.ReportConfig.Format
	}
	outputFile := fmt.Sprintf("/tmp/gosbench_results_%d.%s", time.Now().UnixMilli(), format)
	{
		f, err := os.Create(outputFile)
		if err != nil {
			log.Warningf("Failed to open file %s: %s", outputFile, err)
			t.SetOutputMirror(os.Stdout)
		} else {
			defer f.Close()
			t.SetOutputMirror(f)
		}
		t.AppendHeader(table.Row{"type", "object-size(KB)", "workers", "parallel-clients", "avg-bandwidth(MB/s)", "avg-latency(ms)",
			"gen-bytes-avg-latency(ms)", "io-copy-avg-latency(ms)", "successful-ops", "failed-ops", "duration(s)", "total-mbytes", "name",
		})
		for _, r := range results {
			t.AppendRow(table.Row{
				r.Type.String(),
				r.ObjectSize / 1024,
				r.Workers,
				r.ParallelClients,
				fmt.Sprintf("%.1f", r.BandwidthAvg/1024/1024),
				fmt.Sprintf("%.2f", r.LatencyAvg),
				fmt.Sprintf("%.2f", r.GenBytesLatencyAvg),
				fmt.Sprintf("%.2f", r.IOCopyLatencyAvg),
				r.SuccessfulOperations,
				r.FailedOperations,
				r.Duration.Round(time.Second),
				fmt.Sprintf("%.0f", r.Bytes/1024/1024),
				r.TestName,
			})
		}
		t.SetColumnConfigs([]table.ColumnConfig{
			{Number: 1, Align: text.AlignCenter},
			{Number: 2, Align: text.AlignCenter},
			{Number: 3, Align: text.AlignCenter},
			{Number: 4, Align: text.AlignCenter},
			{Number: 5, Align: text.AlignCenter},
			{Number: 6, Align: text.AlignCenter},
			{Number: 7, Align: text.AlignCenter},
			{Number: 8, Align: text.AlignCenter},
			{Number: 9, Align: text.AlignCenter},
			{Number: 10, Align: text.AlignCenter},
			{Number: 11, Align: text.AlignCenter},
			{Number: 12, Align: text.AlignCenter},
			{Number: 13, Align: text.AlignCenter},
		})
		t.SortBy([]table.SortBy{
			{
				Name: "type",
				Mode: table.Asc,
			},
			{
				Name: "object-size(KB)",
				Mode: table.AscNumeric,
			},
			{
				Name: "parallel-clients",
				Mode: table.AscNumeric,
			},
		})
		switch strings.ToLower(format) {
		case "md", "markdown":
			t.RenderMarkdown()
		case "csv":
			t.RenderCSV()
		case "html":
			t.RenderHTML()
		default:
			t.Render()
		}
	}

	if s.config.ReportConfig == nil || s.config.ReportConfig.Bucket == "" || s.config.ReportConfig.S3Config == nil {
		return
	}

	s3Config := s.config.ReportConfig.S3Config
	ctx := context.Background()
	client, e := common.NewS3Client(ctx, s3Config.Endpoint, s3Config.AccessKey, s3Config.SecretKey, s3Config.Region, s3Config.SkipSSLVerify, true)
	if e != nil {
		log.WithError(e).Warning("Unable to build S3 config to upload report")
		return
	}
	f, e := os.Open(outputFile)
	if e != nil {
		log.WithError(e).Warning("Unable to open report file")
		return
	}
	defer f.Close()
	_, e = client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: &s.config.ReportConfig.Bucket,
		Key:    aws.String(fmt.Sprintf("gosbench_results_%d.%s", time.Now().UnixMilli(), format)),
		Body:   f,
	})
	if e != nil {
		log.WithError(e).Warningf("Failed to upload report file to s3 bucket %s", s.config.ReportConfig.Bucket)
	}
}

func init() {
	log.SetFormatter(&log.TextFormatter{
		FullTimestamp: true,
	})
	rand.New(rand.NewSource(time.Now().UnixNano()))
}
