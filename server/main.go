package main

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"os"
	"time"

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
	for testNumber, test := range s.config.Tests {
		doneChannel := make(chan bool, test.Workers)
		resultChannel := make(chan common.BenchmarkResult, test.Workers)
		continueWorkers := make(chan bool, test.Workers)

		for worker := 0; worker < test.Workers; worker++ {
			workerConfig := &common.WorkerConf{
				Test:      test,
				S3Configs: s.config.S3Configs,
				WorkerID:  fmt.Sprintf("w%d", worker),
				ID:        worker,
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

		log.WithField("test", test.Name).
			WithField("Total Operations", benchResult.Operations).
			WithField("Total Bytes", benchResult.Bytes).
			WithField("Average BW in Byte/s", benchResult.Bandwidth).
			WithField("Average latency in ms", benchResult.LatencyAvg).
			WithField("Gen Bytes Average latency in ms", benchResult.GenBytesLatencyAvg).
			WithField("Workers", benchResult.Workers).
			WithField("Parallel clients", benchResult.ParallelClients).
			WithField("Test runtime on server", benchResult.Duration).
			Infof("PERF RESULTS")
		writeResultToCSV(benchResult)
	}
	log.Info("All performance tests finished")
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
	for _, result := range results {
		sum.Bytes += result.Bytes
		sum.Operations += result.Operations
		latencyAverages += result.LatencyAvg
		genBytesLatencyAverages += result.GenBytesLatencyAvg
		bandwidthAverages += result.Bandwidth
	}
	sum.LatencyAvg = latencyAverages / float64(len(results))
	sum.GenBytesLatencyAvg = genBytesLatencyAverages / float64(len(results))
	sum.TestName = results[0].TestName
	sum.Bandwidth = bandwidthAverages

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
			"Total Operations",
			"Total Bytes",
			"Average Bandwidth in Bytes/s",
			"Average Latency in ms",
			"Gen Bytes Average Latency in ms",
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
		fmt.Sprintf("%.0f", benchResult.Operations),
		fmt.Sprintf("%.0f", benchResult.Bytes),
		fmt.Sprintf("%f", benchResult.Bandwidth),
		fmt.Sprintf("%f", benchResult.LatencyAvg),
		fmt.Sprintf("%f", benchResult.GenBytesLatencyAvg),
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

func init() {
	log.SetFormatter(&log.TextFormatter{
		FullTimestamp: true,
	})
	rand.New(rand.NewSource(time.Now().UnixNano()))
}
