package main

import (
	"fmt"
	"math/rand"
	"sort"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/mulbc/gosbench/common"
)

// WorkItem is an interface for general work operations
// They can be read,write,list,delete or a stopper
type WorkItem interface {
	Prepare(conf *common.TestCaseConfiguration) error
	Do(conf *common.TestCaseConfiguration) error
	Clean() error
}

type BaseOperation struct {
	TestName   string
	Bucket     string
	ObjectName string
	ObjectSize uint64
}

// ReadOperation stands for a read operation
type ReadOperation struct {
	*BaseOperation
	WorksOnPreexistingObject bool
}

// WriteOperation stands for a write operation
type WriteOperation struct {
	*BaseOperation
}

// ListOperation stands for a list operation
type ListOperation struct {
	*BaseOperation
}

// DeleteOperation stands for a delete operation
type DeleteOperation struct {
	*BaseOperation
}

// Stopper marks the end of a workqueue when using
// maxOps as testCase end criterium
type Stopper struct{}

// KV is a simple key-value struct
type KV struct {
	Key   string // read, existing_read, write, list, delete
	Value float64
}

// WorkQueue contains the Queue and the valid operation's
// values to determine which operation should be done next
// in order to satisfy the set ratios.
type WorkQueue struct {
	OperationValues []KV
	Queue           []WorkItem
}

// GetNextOperation evaluates the operation values and returns which
// operation should happen next
func GetNextOperation(queue *WorkQueue) string {
	sort.Slice(queue.OperationValues, func(i, j int) bool {
		return queue.OperationValues[i].Value < queue.OperationValues[j].Value
	})
	return queue.OperationValues[0].Key
}

// IncreaseOperationValue increases the given operation's value by the set amount
func IncreaseOperationValue(operation string, value float64, queue *WorkQueue) error {
	for i := range queue.OperationValues {
		if queue.OperationValues[i].Key == operation {
			queue.OperationValues[i].Value += value
			return nil
		}
	}
	return fmt.Errorf("Could not find requested operation %s", operation)
}

// Prepare prepares the execution of the ReadOperation
func (op *ReadOperation) Prepare(conf *common.TestCaseConfiguration) error {
	log.WithField("bucket", op.Bucket).WithField("object", op.ObjectName).WithField("Preexisting?", op.WorksOnPreexistingObject).Debug("Preparing ReadOperation")
	if op.WorksOnPreexistingObject {
		return nil
	}
	return putObject(housekeepingSvc, conf, op.BaseOperation)
}

// Prepare prepares the execution of the WriteOperation
func (op *WriteOperation) Prepare(conf *common.TestCaseConfiguration) error {
	log.WithField("bucket", op.Bucket).WithField("object", op.ObjectName).Debug("Preparing WriteOperation")
	return nil
}

// Prepare prepares the execution of the ListOperation
func (op *ListOperation) Prepare(conf *common.TestCaseConfiguration) error {
	log.WithField("bucket", op.Bucket).WithField("object", op.ObjectName).Debug("Preparing ListOperation")
	return putObject(housekeepingSvc, conf, op.BaseOperation)
}

// Prepare prepares the execution of the DeleteOperation
func (op *DeleteOperation) Prepare(conf *common.TestCaseConfiguration) error {
	log.WithField("bucket", op.Bucket).WithField("object", op.ObjectName).Debug("Preparing DeleteOperation")
	// check whether object is exist or not
	_, err := headObject(housekeepingSvc, op.ObjectName, op.Bucket)
	// object already exist
	if err == nil {
		return nil
	}
	return putObject(housekeepingSvc, conf, op.BaseOperation)
}

// Prepare does nothing here
func (op *Stopper) Prepare(conf *common.TestCaseConfiguration) error {
	return nil
}

// Do executes the actual work of the ReadOperation
func (op *ReadOperation) Do(conf *common.TestCaseConfiguration) error {
	log.WithField("bucket", op.Bucket).WithField("object", op.ObjectName).WithField("Preexisting?", op.WorksOnPreexistingObject).Debug("Doing ReadOperation")
	start := time.Now()
	err := getObject(svc, conf, op.BaseOperation)
	duration := time.Since(start)
	promLatency.WithLabelValues(op.TestName, "GET").Observe(float64(duration.Milliseconds()))
	if err != nil {
		promFailedOps.WithLabelValues(op.TestName, "GET").Inc()
	} else {
		promFinishedOps.WithLabelValues(op.TestName, "GET").Inc()
	}
	promDownloadedBytes.WithLabelValues(op.TestName, "GET").Add(float64(op.ObjectSize))
	return err
}

// Do executes the actual work of the WriteOperation
func (op *WriteOperation) Do(conf *common.TestCaseConfiguration) error {
	log.WithField("bucket", op.Bucket).WithField("object", op.ObjectName).Debug("Doing WriteOperation")
	err := putObject(svc, conf, op.BaseOperation)
	if err != nil {
		promFailedOps.WithLabelValues(op.TestName, "PUT").Inc()
	} else {
		promFinishedOps.WithLabelValues(op.TestName, "PUT").Inc()
	}
	promUploadedBytes.WithLabelValues(op.TestName, "PUT").Add(float64(op.ObjectSize))
	return err
}

// Do executes the actual work of the ListOperation
func (op *ListOperation) Do(conf *common.TestCaseConfiguration) error {
	log.WithField("bucket", op.Bucket).WithField("object", op.ObjectName).Debug("Doing ListOperation")
	start := time.Now()
	_, err := listObjects(svc, op.ObjectName, op.Bucket)
	duration := time.Since(start)
	promLatency.WithLabelValues(op.TestName, "LIST").Observe(float64(duration.Milliseconds()))
	if err != nil {
		promFailedOps.WithLabelValues(op.TestName, "LIST").Inc()
	} else {
		promFinishedOps.WithLabelValues(op.TestName, "LIST").Inc()
	}
	return err
}

// Do executes the actual work of the DeleteOperation
func (op *DeleteOperation) Do(conf *common.TestCaseConfiguration) error {
	log.WithField("bucket", op.Bucket).WithField("object", op.ObjectName).Debug("Doing DeleteOperation")
	start := time.Now()
	err := deleteObject(svc, op.ObjectName, op.Bucket)
	duration := time.Since(start)
	promLatency.WithLabelValues(op.TestName, "DELETE").Observe(float64(duration.Milliseconds()))
	if err != nil {
		promFailedOps.WithLabelValues(op.TestName, "DELETE").Inc()
	} else {
		promFinishedOps.WithLabelValues(op.TestName, "DELETE").Inc()
	}
	return err
}

// Do does nothing here
func (op *Stopper) Do(conf *common.TestCaseConfiguration) error {
	return nil
}

// Clean removes the objects and buckets left from the previous ReadOperation
func (op *ReadOperation) Clean() error {
	if op.WorksOnPreexistingObject {
		return nil
	}
	log.WithField("bucket", op.Bucket).WithField("object", op.ObjectName).WithField("Preexisting?", op.WorksOnPreexistingObject).Debug("Cleaning up ReadOperation")
	return deleteObject(housekeepingSvc, op.ObjectName, op.Bucket)
}

// Clean removes the objects and buckets left from the previous WriteOperation
func (op *WriteOperation) Clean() error {
	return deleteObject(housekeepingSvc, op.ObjectName, op.Bucket)
}

// Clean removes the objects and buckets left from the previous ListOperation
func (op *ListOperation) Clean() error {
	return deleteObject(housekeepingSvc, op.ObjectName, op.Bucket)
}

// Clean removes the objects and buckets left from the previous DeleteOperation
func (op *DeleteOperation) Clean() error {
	return nil
}

// Clean does nothing here
func (op *Stopper) Clean() error {
	return nil
}

// DoWork processes the workitems in the workChannel until
// either the time runs out or a stopper is found
func (w *Worker) DoWork(workChannel <-chan WorkItem, notifyChan <-chan struct{}, wg *sync.WaitGroup) {
	defer wg.Done()
	for {
		select {
		case <-notifyChan:
			log.Debugf("Runtime over - Got timeout from work context")
			return
		case work := <-workChannel:
			switch work.(type) {
			case *Stopper:
				log.Debug("Found the end of the work Queue - stopping")
				return
			}
			err := work.Do(w.config.Test)
			if err != nil {
				log.WithError(err).Error("Issues when performing work - ignoring")
			}
		}
	}
}

func generateBytes(payloadGenerator, testName string, size uint64) []byte {
	start := time.Now()
	data := make([]byte, size)
	switch payloadGenerator {
	case "empty": // https://github.com/dvassallo/s3-benchmark/blob/aebfe8e05c1553f35c16362b4ac388d891eee440/main.go#L290-L297
	// case "random":
	default:
		randASCIIBytes(data, rng)
	}
	duration := time.Since(start)
	promGenBytesLatency.WithLabelValues(testName).Observe(float64(duration.Milliseconds()))
	promGenBytesSize.WithLabelValues(testName).Set(float64(size))

	return data
}

// https://github.com/minio/warp/blob/master/pkg/generator/generator.go
const asciiLetters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ1234567890()"

var (
	asciiLetterBytes [len(asciiLetters)]byte
	rng              *rand.Rand
)

// randASCIIBytes fill destination with pseudorandom ASCII characters [a-ZA-Z0-9].
// Should never be considered for true random data generation.
func randASCIIBytes(dst []byte, rng *rand.Rand) {
	// Use a single seed.
	v := rng.Uint64()
	rnd := uint32(v)
	rnd2 := uint32(v >> 32)
	for i := range dst {
		dst[i] = asciiLetterBytes[int(rnd>>16)%len(asciiLetterBytes)]
		rnd ^= rnd2
		rnd *= 2654435761
	}
}

func init() {
	for i, v := range asciiLetters {
		asciiLetterBytes[i] = byte(v)
	}
	rndSrc := rand.NewSource(int64(rand.Uint64()))
	rng = rand.New(rndSrc)
}
