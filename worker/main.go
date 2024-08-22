package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math/rand"
	"net"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/mulbc/gosbench/common"
	log "github.com/sirupsen/logrus"
)

var prometheusPort int
var debug, trace bool

func init() {
	runtime.GOMAXPROCS(runtime.NumCPU())
	log.SetFormatter(&log.TextFormatter{
		FullTimestamp: true,
	})
	rand.Seed(time.Now().UnixNano())
}

func main() {
	var serverAddress string
	flag.StringVar(&serverAddress, "s", "", "Gosbench Server IP and Port in the form '192.168.1.1:2000'")
	flag.IntVar(&prometheusPort, "p", 8888, "Port on which the Prometheus Exporter will be available. Default: 8888")
	flag.BoolVar(&debug, "d", false, "enable debug log output")
	flag.BoolVar(&trace, "t", false, "enable trace log output")
	flag.Parse()
	if serverAddress == "" {
		log.Fatal("-s is a mandatory parameter - please specify the server IP and Port")
	}

	if debug {
		log.SetLevel(log.DebugLevel)
	} else if trace {
		log.SetLevel(log.TraceLevel)
	} else {
		log.SetLevel(log.InfoLevel)
	}

	worker := &Worker{
		Workqueue: &Workqueue{
			Queue: &[]WorkItem{},
		},
	}
	for {
		err := worker.connectToServer(serverAddress)
		if err != nil {
			log.WithError(err).Error("Issues with server connection")
			time.Sleep(time.Second)
		}
	}
}

type Worker struct {
	Workqueue       *Workqueue
	ParallelClients int
	config          common.WorkerConf
}

func (w *Worker) connectToServer(serverAddress string) error {
	conn, err := net.Dial("tcp", serverAddress)
	if err != nil {
		// return errors.New("Could not establish connection to server yet")
		return err
	}
	encoder := json.NewEncoder(conn)
	decoder := json.NewDecoder(conn)

	_ = encoder.Encode("ready for work")

	var response common.WorkerMessage
	for {
		err := decoder.Decode(&response)
		if err != nil {
			log.WithField("message", response).WithError(err).Error("Server responded unusually - reconnecting")
			conn.Close()
			return errors.New("Issue when receiving work from server")
		}
		log.Tracef("Response: %+v", response)
		switch response.Message {
		case "init":
			config := *response.Config
			w.config = config
			w.ParallelClients = w.config.Test.ParallelClients
			log.Info("Got config from server - starting preparations now")

			InitS3(*config.S3Config)
			w.fillWorkqueue(config.WorkerID, config.Test.WorkerShareBuckets)

			if !config.Test.SkipPrepare {
				for _, work := range *w.Workqueue.Queue {
					err = work.Prepare(config.Test)
					if err != nil {
						log.WithError(err).Error("Error during work preparation - ignoring")
					}
				}
			}
			log.Info("Preparations finished - waiting on server to start work")
			_ = encoder.Encode(common.WorkerMessage{Message: "preparations done"})
		case "start work":
			if w.config == (common.WorkerConf{}) || len(*w.Workqueue.Queue) == 0 {
				log.Fatal("Was instructed to start work - but the preparation step is incomplete - reconnecting")
				return nil
			}
			log.Info("Starting to work")
			duration := w.PerfTest()
			benchResults := w.getCurrentPromValues()
			benchResults.Duration = duration
			benchResults.Bandwidth = benchResults.Bytes / duration.Seconds()
			log.Infof("PROM VALUES %+v", benchResults)
			_ = encoder.Encode(common.WorkerMessage{Message: "work done", BenchResult: benchResults})
			// Work is done - return to being a ready worker by reconnecting
			return nil
		case "shutdown":
			log.Info("Server told us to shut down - all work is done for today")
			os.Exit(0)
		}
	}
}

// PerfTest runs a performance test as configured in testConfig
func (w *Worker) PerfTest() time.Duration {
	workerID := w.config.WorkerID
	testConfig := w.config.Test
	workChannel := make(chan WorkItem, len(*w.Workqueue.Queue))
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
	if testConfig.Timeout != 0 {
		w.workUntilTimeout(workChannel, notifyChan, time.Duration(testConfig.Timeout), true)
	} else {
		if testConfig.Runtime != 0 {
			w.workUntilTimeout(workChannel, notifyChan, time.Duration(testConfig.Runtime), false)
		} else {
			w.workUntilOps(workChannel, testConfig.OpsDeadline)
		}
	}
	// Wait for all the goroutines to finish
	wg.Wait()
	log.Info("All clients finished")
	endTime := time.Now().UTC()
	promTestEnd.WithLabelValues(testConfig.Name).Set(float64(endTime.UnixNano() / int64(1000000)))

	if testConfig.CleanAfter {
		log.Info("Housekeeping started")
		for _, work := range *w.Workqueue.Queue {
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

func (w *Worker) workUntilTimeout(workChannel chan WorkItem, notifyChan chan<- struct{}, runtime time.Duration, breakLoop bool) {
	timer := time.NewTimer(runtime)
	for {
		log.Debugf("The length of work queue: %d", len(*w.Workqueue.Queue))
		for _, work := range *w.Workqueue.Queue {
			select {
			case <-timer.C:
				log.Debug("Reached Runtime end")
				close(notifyChan)
				return
			case workChannel <- work:
			}
		}
		if !w.config.Test.SkipPrepare {
			for _, work := range *w.Workqueue.Queue {
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
		if breakLoop {
			log.Debug("Reached limit of work ops... waiting for workers to finish")
			for worker := 0; worker < w.ParallelClients; worker++ {
				workChannel <- &Stopper{}
			}
			return
		}
	}
}

func (w *Worker) workUntilOps(workChannel chan WorkItem, maxOps uint64) {
	currentOps := uint64(0)
	for {
		for _, work := range *w.Workqueue.Queue {
			if currentOps >= maxOps {
				log.Debug("Reached OpsDeadline ... waiting for workers to finish")
				for worker := 0; worker < w.ParallelClients; worker++ {
					workChannel <- &Stopper{}
				}
				return
			}
			currentOps++
			workChannel <- work
		}

		remainOps := maxOps - currentOps
		for _, work := range *w.Workqueue.Queue {
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

func (w *Worker) fillWorkqueue(workerID string, shareBucketName bool) {
	testConfig := w.config.Test

	if testConfig.ReadWeight > 0 {
		w.Workqueue.OperationValues = append(w.Workqueue.OperationValues, KV{Key: "read"})
	}
	if testConfig.ExistingReadWeight > 0 {
		w.Workqueue.OperationValues = append(w.Workqueue.OperationValues, KV{Key: "existing_read"})
	}
	if testConfig.WriteWeight > 0 {
		w.Workqueue.OperationValues = append(w.Workqueue.OperationValues, KV{Key: "write"})
	}
	if testConfig.ListWeight > 0 {
		w.Workqueue.OperationValues = append(w.Workqueue.OperationValues, KV{Key: "list"})
	}
	if testConfig.DeleteWeight > 0 {
		w.Workqueue.OperationValues = append(w.Workqueue.OperationValues, KV{Key: "delete"})
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

			nextOp := GetNextOperation(w.Workqueue)
			switch nextOp {
			case "read":
				err := IncreaseOperationValue(nextOp, 1/float64(testConfig.ReadWeight), w.Workqueue)
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
				*w.Workqueue.Queue = append(*w.Workqueue.Queue, new)
			case "existing_read":
				err := IncreaseOperationValue(nextOp, 1/float64(testConfig.ExistingReadWeight), w.Workqueue)
				if err != nil {
					log.WithError(err).Error("Could not increase operational Value - ignoring")
				}
				new := &ReadOperation{
					BaseOperation: &BaseOperation{
						TestName:   testConfig.Name,
						Bucket:     bucketName,
						ObjectName: *preExistingObjects.Contents[object%preExistingObjectCount].Key,
						ObjectSize: uint64(*preExistingObjects.Contents[object%preExistingObjectCount].Size),
					},
					WorksOnPreexistingObject: true,
				}
				*w.Workqueue.Queue = append(*w.Workqueue.Queue, new)
			case "write":
				err := IncreaseOperationValue(nextOp, 1/float64(testConfig.WriteWeight), w.Workqueue)
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
				*w.Workqueue.Queue = append(*w.Workqueue.Queue, new)
			case "list":
				err := IncreaseOperationValue(nextOp, 1/float64(testConfig.ListWeight), w.Workqueue)
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
				*w.Workqueue.Queue = append(*w.Workqueue.Queue, new)
			case "delete":
				err := IncreaseOperationValue(nextOp, 1/float64(testConfig.DeleteWeight), w.Workqueue)
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
				*w.Workqueue.Queue = append(*w.Workqueue.Queue, new)
			}
		}
	}
}
