package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/mulbc/gosbench/common"
)

const (
	gosbenchInContainer = "GOSBENCH_IN_CONTAINER"
)

var (
	prometheusPort int
	debug, trace   bool
	serverAddress  string
)

func main() {
	rootCmd := newCommand()
	cobra.CheckErr(rootCmd.Execute())
}

func newCommand() *cobra.Command {
	cmds := &cobra.Command{
		Use: "gosbench-worker",
		Run: func(cmd *cobra.Command, args []string) {
			run()
		},
	}
	cmds.Flags().SortFlags = false

	viper.SetDefault("DEBUG", false)
	viper.SetDefault("TRACE", false)
	viper.SetDefault("PROMETHEUSPORT", 8888)

	viper.AutomaticEnv()
	viper.AllowEmptyEnv(true)

	cmds.Flags().BoolVar(&trace, "trace", viper.GetBool("TRACE"), "enable trace log output")
	cmds.Flags().BoolVar(&debug, "debug", viper.GetBool("DEBUG"), "enable debug log output")
	cmds.Flags().StringVar(&serverAddress, "server.address", viper.GetString("SERVERADDRESS"), "Gosbench Server IP and Port in the form '191.168.1.1:2000'")
	cmds.Flags().IntVar(&prometheusPort, "prometheus.port", viper.GetInt("PROMETHEUSPORT"), "Port on which the Prometheus Exporter will be available. Default: 8888")

	return cmds
}

func run() {
	if serverAddress == "" {
		log.Fatal("--server.address is a mandatory parameter - please specify the server IP and Port")
	}

	if debug {
		log.SetLevel(log.DebugLevel)
	} else if trace {
		log.SetLevel(log.TraceLevel)
	} else {
		log.SetLevel(log.InfoLevel)
	}
	log.Debugf("viper settings=%+v", viper.AllSettings())
	log.Debugf("gosbench worker serverAddress=%s, prometheusPort=%d", serverAddress, prometheusPort)

	go func() {
		mux := http.NewServeMux()
		mux.Handle("/metrics", newHandler())
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`<html>
             <head><title>Gosbench Exporter</title></head>
             <body>
             <h1>Gosbench Exporter</h1>
             <p><a href="` + `/metrics` + `">Metrics</a></p>
             </body>
             </html>`))
		})
		// http://localhost:8888/metrics
		log.Infof("Starting Prometheus Exporter on port %d", prometheusPort)
		if err := http.ListenAndServe(fmt.Sprintf(":%d", prometheusPort), mux); err != nil {
			log.WithError(err).Fatalf("Failed to run Prometheus /metrics endpoint:")
		}
	}()

	worker := &Worker{
		workQueue: &WorkQueue{},
	}
	for {
		err := worker.connectToServer(serverAddress)
		if err != nil {
			log.WithError(err).Error("Issues with server connection")
			time.Sleep(time.Second)
		} else {
			if os.Getenv(gosbenchInContainer) == "true" {
				select {} // sleep forever in container!
			} else {
				os.Exit(0)
			}
		}
	}
}

type Worker struct {
	workQueue       *WorkQueue
	parallelClients int
	config          common.WorkerConf
}

func (w *Worker) connectToServer(serverAddress string) error {
	conn, err := net.Dial("tcp", serverAddress)
	if err != nil {
		// return errors.New("Could not establish connection to server yet")
		return err
	}
	defer conn.Close()
	encoder := json.NewEncoder(conn)
	decoder := json.NewDecoder(conn)

	_ = encoder.Encode("ready for work")

	var response common.WorkerMessage
	for {
		err := decoder.Decode(&response)
		if err != nil {
			log.WithField("message", response).WithError(err).Error("Server responded unusually - reconnecting")
			return errors.New("Issue when receiving work from server")
		}
		log.Tracef("Response: %+v", response)
		switch response.Message {
		case "init":
			config := *response.Config
			w.config = config
			w.parallelClients = w.config.Test.ParallelClients
			w.workQueue.Queue = nil // reset work queue
			log.Infof("Got config %+v from server - starting preparations now", config.Test)

			w.initS3()
			w.fillWorkqueue()

			if !config.Test.SkipPrepare {
				for _, work := range w.workQueue.Queue {
					e := work.Prepare(config.Test)
					if e != nil {
						log.WithError(e).Error("Error during work preparation - ignoring")
					}
				}
			}
			log.Info("Preparations finished - waiting on server to start work")
			err = encoder.Encode(common.WorkerMessage{Message: "preparations done"})
			if err != nil {
				log.WithError(err).Error("Sending preparations done message to server- reconnecting")
				return errors.New("Issue when sending preparations done to server")
			}
		case "start work":
			if len(w.workQueue.Queue) == 0 {
				log.Warningf("Was instructed to start work - but the preparation step is incomplete - reconnecting")
				return nil
			}
			log.Info("Starting to work")
			duration := w.perfTest()
			benchResults := w.getCurrentPromValues()
			benchResults.Duration = duration
			benchResults.Bandwidth = benchResults.Bytes / duration.Seconds()
			log.Infof("PROM VALUES %+v", benchResults)
			err = encoder.Encode(common.WorkerMessage{Message: "work done", BenchResult: benchResults})
			if err != nil {
				log.WithError(err).Error("Sending work done message to server- reconnecting")
				return errors.New("Issue when sending work done to server")
			}
		case "shutdown":
			log.Info("Server told us to shut down - all work is done for today")
			return nil
		}
	}
}

// perfTest runs a performance test as configured in testConfig
func (w *Worker) perfTest() time.Duration {
	workerID := w.config.WorkerID
	testConfig := w.config.Test
	workChannel := make(chan WorkItem, len(w.workQueue.Queue))
	notifyChan := make(chan struct{})
	wg := &sync.WaitGroup{}
	wg.Add(testConfig.ParallelClients)

	startTime := time.Now().UTC()
	promTestStart.WithLabelValues(testConfig.Name).Set(float64(startTime.UnixNano() / int64(1000000)))
	// promTestGauge.WithLabelValues(testConfig.Name).Inc()
	for worker := 0; worker < testConfig.ParallelClients; worker++ {
		go w.DoWork(workChannel, notifyChan, wg)
	}
	log.Infof("Started %d parallel clients", testConfig.ParallelClients)
	if testConfig.Runtime != 0 {
		w.workUntilTimeout(workChannel, notifyChan, time.Duration(testConfig.Runtime))
	} else if testConfig.OpsDeadline != 0 {
		w.workUntilOps(workChannel, testConfig.OpsDeadline)
	} else {
		w.workOneShot(workChannel)
	}
	// Wait for all the goroutines to finish
	wg.Wait()

	log.Info("All clients finished")
	endTime := time.Now().UTC()
	promTestEnd.WithLabelValues(testConfig.Name).Set(float64(endTime.UnixNano() / int64(1000000)))

	if testConfig.CleanAfter {
		log.Info("Housekeeping started")
		for _, work := range w.workQueue.Queue {
			err := work.Clean()
			if err != nil {
				log.WithError(err).Error("Error during cleanup - ignoring")
			}
		}
		for bucket := testConfig.Buckets.NumberMin; bucket <= testConfig.Buckets.NumberMax; bucket++ {
			bucketName := fmt.Sprintf("%s%s%d", workerID, testConfig.BucketPrefix, bucket)
			if testConfig.WorkerShareBuckets {
				bucketName = fmt.Sprintf("%s%d", testConfig.BucketPrefix, bucket)
			}
			err := deleteBucket(housekeepingSvc, bucketName)
			if err != nil {
				log.WithError(err).Error("Error during bucket deleting - ignoring")
			}
		}
		log.Info("Housekeeping finished")
	}
	// Sleep to ensure Prometheus can still scrape the last information before we restart the worker
	time.Sleep(10 * time.Second)
	return endTime.Sub(startTime)
}

func (w *Worker) workUntilTimeout(workChannel chan WorkItem, notifyChan chan<- struct{}, runtime time.Duration) {
	timer := time.NewTimer(runtime)
	for {
		log.Debugf("The length of work queue: %d", len(w.workQueue.Queue))
		for _, work := range w.workQueue.Queue {
			select {
			case <-timer.C:
				log.Debug("Reached Runtime end")
				close(notifyChan)
				return
			case workChannel <- work:
			}
		}
		if !w.config.Test.SkipPrepare {
			for _, work := range w.workQueue.Queue {
				switch work.(type) {
				case *DeleteOperation:
					log.Debug("Re-Running Work preparation for delete job started")
					err := work.Prepare(w.config.Test)
					if err != nil {
						log.WithError(err).Error("Error during work preparation - ignoring")
					}
					log.Debug("Delete preparation re-run finished")
				}
			}
		} else {
			log.Debug("Skip to delete preparation re-run finished")
		}
	}
}

func (w *Worker) workOneShot(workChannel chan WorkItem) {
	for _, work := range w.workQueue.Queue {
		workChannel <- work
	}
	for worker := 0; worker < w.parallelClients; worker++ {
		workChannel <- &Stopper{}
	}
}

func (w *Worker) workUntilOps(workChannel chan WorkItem, maxOps uint64) {
	currentOps := uint64(0)
	for {
		for _, work := range w.workQueue.Queue {
			if currentOps >= maxOps {
				log.Debug("Reached OpsDeadline ... waiting for workers to finish")
				for worker := 0; worker < w.parallelClients; worker++ {
					workChannel <- &Stopper{}
				}
				return
			}
			currentOps++
			workChannel <- work
		}

		remainOps := maxOps - currentOps
		for _, work := range w.workQueue.Queue {
			if remainOps <= 0 {
				break
			}
			remainOps--
			switch work.(type) {
			case *DeleteOperation:
				log.Debug("Re-Running Work preparation for delete job started")
				if !w.config.Test.SkipPrepare {
					err := work.Prepare(w.config.Test)
					if err != nil {
						log.WithError(err).Error("Error during work preparation - ignoring")
					}
					log.Debug("Delete preparation re-run finished")
				}
				log.Debug("Skip to delete preparation re-run finished")
			}
		}
	}
}

func (w *Worker) fillWorkqueue() {
	workerID := w.config.WorkerID
	shareBucketName := w.config.Test.WorkerShareBuckets
	testConfig := w.config.Test

	if testConfig.ReadWeight > 0 {
		w.workQueue.OperationValues = append(w.workQueue.OperationValues, KV{Key: "read"})
	}
	if testConfig.ExistingReadWeight > 0 {
		w.workQueue.OperationValues = append(w.workQueue.OperationValues, KV{Key: "existing_read"})
	}
	if testConfig.WriteWeight > 0 {
		w.workQueue.OperationValues = append(w.workQueue.OperationValues, KV{Key: "write"})
	}
	if testConfig.ListWeight > 0 {
		w.workQueue.OperationValues = append(w.workQueue.OperationValues, KV{Key: "list"})
	}
	if testConfig.DeleteWeight > 0 {
		w.workQueue.OperationValues = append(w.workQueue.OperationValues, KV{Key: "delete"})
	}

	for bucketn := testConfig.Buckets.NumberMin; bucketn <= testConfig.Buckets.NumberMax; bucketn++ {
		bucket := common.EvaluateDistribution(testConfig.Buckets.NumberMin, testConfig.Buckets.NumberMax, &testConfig.Buckets.NumberLast, 1, testConfig.Buckets.NumberDistribution)

		bucketName := fmt.Sprintf("%s%s%d", workerID, testConfig.BucketPrefix, bucket)
		if shareBucketName {
			bucketName = fmt.Sprintf("%s%d", testConfig.BucketPrefix, bucket)
		}
		err := createBucket(housekeepingSvc, bucketName)
		if err != nil {
			log.WithError(err).WithField("bucket", bucketName).Error("Error when creating bucket")
		}
		var preExistingObjects []types.Object
		var preExistingObjectCount uint64
		if testConfig.ExistingReadWeight > 0 {
			preExistingObjects, err = listObjects(housekeepingSvc, "", bucketName)
			if err != nil {
				log.WithError(err).Fatalf("Problems when listing contents of bucket %s", bucketName)
			}
			preExistingObjectCount = uint64(len(preExistingObjects))
			log.Debugf("Found %d objects in bucket %s", preExistingObjectCount, bucketName)

			if preExistingObjectCount <= 0 {
				log.Warningf("There is no objects in bucket %s", bucketName)
				continue
			}
		}

		for objectn := testConfig.Objects.NumberMin; objectn <= testConfig.Objects.NumberMax; objectn++ {
			object := common.EvaluateDistribution(testConfig.Objects.NumberMin, testConfig.Objects.NumberMax, &testConfig.Objects.NumberLast, 1, testConfig.Objects.NumberDistribution)
			objectSize := common.EvaluateDistribution(testConfig.Objects.SizeMin, testConfig.Objects.SizeMax, &testConfig.Objects.SizeLast, 1, testConfig.Objects.SizeDistribution)

			nextOp := GetNextOperation(w.workQueue)
			switch nextOp {
			case "read":
				err := IncreaseOperationValue(nextOp, 1/float64(testConfig.ReadWeight), w.workQueue)
				if err != nil {
					log.WithError(err).Error("Could not increase operational Value - ignoring")
				}
				new := &ReadOperation{
					BaseOperation: &BaseOperation{
						TestName:   testConfig.Name,
						Bucket:     bucketName,
						ObjectName: fmt.Sprintf("%s%s%d", workerID, testConfig.ObjectPrefix, object),
						ObjectSize: objectSize,
					},
					WorksOnPreexistingObject: false,
				}
				w.workQueue.Queue = append(w.workQueue.Queue, new)
			case "existing_read":
				err := IncreaseOperationValue(nextOp, 1/float64(testConfig.ExistingReadWeight), w.workQueue)
				if err != nil {
					log.WithError(err).Error("Could not increase operational Value - ignoring")
				}
				new := &ReadOperation{
					BaseOperation: &BaseOperation{
						TestName:   testConfig.Name,
						Bucket:     bucketName,
						ObjectName: *preExistingObjects[object%preExistingObjectCount].Key,
						ObjectSize: uint64(*preExistingObjects[object%preExistingObjectCount].Size),
					},
					WorksOnPreexistingObject: true,
				}
				w.workQueue.Queue = append(w.workQueue.Queue, new)
			case "write":
				err := IncreaseOperationValue(nextOp, 1/float64(testConfig.WriteWeight), w.workQueue)
				if err != nil {
					log.WithError(err).Error("Could not increase operational Value - ignoring")
				}
				new := &WriteOperation{
					BaseOperation: &BaseOperation{
						TestName:   testConfig.Name,
						Bucket:     bucketName,
						ObjectName: fmt.Sprintf("%s%s%d", workerID, testConfig.ObjectPrefix, object),
						ObjectSize: objectSize,
					},
				}
				w.workQueue.Queue = append(w.workQueue.Queue, new)
			case "list":
				err := IncreaseOperationValue(nextOp, 1/float64(testConfig.ListWeight), w.workQueue)
				if err != nil {
					log.WithError(err).Error("Could not increase operational Value - ignoring")
				}
				new := &ListOperation{
					BaseOperation: &BaseOperation{
						TestName:   testConfig.Name,
						Bucket:     bucketName,
						ObjectName: fmt.Sprintf("%s%s%d", workerID, testConfig.ObjectPrefix, object),
						ObjectSize: objectSize,
					},
				}
				w.workQueue.Queue = append(w.workQueue.Queue, new)
			case "delete":
				err := IncreaseOperationValue(nextOp, 1/float64(testConfig.DeleteWeight), w.workQueue)
				if err != nil {
					log.WithError(err).Error("Could not increase operational Value - ignoring")
				}
				new := &DeleteOperation{
					BaseOperation: &BaseOperation{
						TestName:   testConfig.Name,
						Bucket:     bucketName,
						ObjectName: fmt.Sprintf("%s%s%d", workerID, testConfig.ObjectPrefix, object),
						ObjectSize: objectSize,
					},
				}
				w.workQueue.Queue = append(w.workQueue.Queue, new)
			}
		}
	}
}

func init() {
	runtime.GOMAXPROCS(runtime.NumCPU())
	log.SetFormatter(&log.TextFormatter{
		FullTimestamp: true,
	})
	rand.New(rand.NewSource(time.Now().UnixNano()))
}
