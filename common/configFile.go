package common

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	log "github.com/sirupsen/logrus"
)

// This uses the Base 2 calculation where
// 1 kB = 1024 Byte
const (
	BYTE = 1 << (10 * iota)
	KILOBYTE
	MEGABYTE
	GIGABYTE
	TERABYTE
)

// S3Configuration contains all information to connect to a certain S3 endpoint
type S3Configuration struct {
	AccessKey     string        `yaml:"access_key" json:"access_key"`
	SecretKey     string        `yaml:"secret_key" json:"secret_key"`
	Region        string        `yaml:"region" json:"region"`
	Endpoint      string        `yaml:"endpoint" json:"endpoint"`
	Timeout       time.Duration `yaml:"timeout" json:"timeout"`
	SkipSSLVerify bool          `yaml:"skipSSLverify" json:"skipSSLverify"`
	Name          string        `yaml:"name" json:"name"`
}

// GrafanaConfiguration contains all information necessary to add annotations
// via the Grafana HTTP API
type GrafanaConfiguration struct {
	Username string `yaml:"username" json:"username"`
	Password string `yaml:"password" json:"password"`
	Endpoint string `yaml:"endpoint" json:"endpoint"`
}

// TestCaseConfiguration is the configuration of a performance test
type TestCaseConfiguration struct {
	Objects struct {
		SizeMin            uint64 `yaml:"size_min" json:"size_min"`
		SizeMax            uint64 `yaml:"size_max" json:"size_max"`
		SizeLast           uint64
		SizeDistribution   string `yaml:"size_distribution" json:"size_distribution"`
		NumberMin          uint64 `yaml:"number_min" json:"number_min"`
		NumberMax          uint64 `yaml:"number_max" json:"number_max"`
		NumberLast         uint64
		NumberDistribution string `yaml:"number_distribution" json:"number_distribution"`
		Unit               string `yaml:"unit" json:"unit"`
	} `yaml:"objects" json:"objects"`
	Buckets struct {
		NumberMin          uint64 `yaml:"number_min" json:"number_min"`
		NumberMax          uint64 `yaml:"number_max" json:"number_max"`
		NumberLast         uint64
		NumberDistribution string `yaml:"number_distribution" json:"number_distribution"`
	} `yaml:"buckets" json:"buckets"`
	Name               string    `yaml:"name" json:"name"`
	BucketPrefix       string    `yaml:"bucket_prefix" json:"bucket_prefix"`
	ObjectPrefix       string    `yaml:"object_prefix" json:"object_prefix"`
	Runtime            Duration  `yaml:"stop_with_runtime" json:"stop_with_runtime"`
	OpsDeadline        uint64    `yaml:"stop_with_ops" json:"stop_with_ops"`
	OneShot            bool      `yaml:"oneshot" json:"oneshot"`
	Workers            int       `yaml:"workers" json:"workers"`
	WorkerShareBuckets bool      `yaml:"workers_share_buckets" json:"workers_share_buckets"`
	SkipPrepare        bool      `yaml:"skip_prepare" json:"skip_prepare"`
	ParallelClients    int       `yaml:"parallel_clients" json:"parallel_clients"`
	CleanAfter         bool      `yaml:"clean_after" json:"clean_after"`
	ReadWeight         int       `yaml:"read_weight" json:"read_weight"`
	ExistingReadWeight int       `yaml:"existing_read_weight" json:"existing_read_weight"`
	WriteWeight        int       `yaml:"write_weight" json:"write_weight"`
	ListWeight         int       `yaml:"list_weight" json:"list_weight"`
	DeleteWeight       int       `yaml:"delete_weight" json:"delete_weight"`
	WriteOption        *S3Option `yaml:"write_option" json:"write_option"`
	ReadOption         *S3Option `yaml:"read_option" json:"read_option"`
	PayloadGenerator   string    `yaml:"payload_generator" json:"payload_generator"` // empty or random
}

type S3Option struct {
	MaxUploadParts int32  `yaml:"max_upload_parts" json:"max_upload_parts"` // only for write
	Concurrency    int    `yaml:"concurrency" json:"concurrency"`
	ChunkSize      int64  `yaml:"chunk_size" json:"chunk_size"`
	Unit           string `yaml:"unit" json:"unit"`
}

// TestConf contains all the information necessary to set up a distributed test
type TestConf struct {
	S3Configs     []*S3Configuration       `yaml:"s3_configs" json:"s3_configs"`
	GrafanaConfig *GrafanaConfiguration    `yaml:"grafana_config" json:"grafana_config"`
	Tests         []*TestCaseConfiguration `yaml:"tests" json:"tests"`
}

// WorkerConf is the configuration that is sent to each worker
// It includes a subset of information from the Testconf
type WorkerConf struct {
	S3Configs []*S3Configuration
	Test      *TestCaseConfiguration
	WorkerID  string
	ID        int
}

// BenchResult is the struct that will contain the benchmark results from a
// worker after it has finished its benchmark
type BenchmarkResult struct {
	TestName        string
	Operations      float64
	Workers         float64
	ParallelClients float64
	Bytes           float64
	// Bandwidth is the amount of Bytes per second of runtime
	Bandwidth          float64
	LatencyAvg         float64
	GenBytesLatencyAvg float64
	Duration           time.Duration
}

// WorkerMessage is the struct that is exchanged in the communication between
// server and worker. It usually only contains a message, but during the init
// phase, also contains the config for the worker
type WorkerMessage struct {
	Message     string
	Config      *WorkerConf
	BenchResult BenchmarkResult
}

// CheckConfig checks the global config
func CheckConfig(config *TestConf) {
	for _, testcase := range config.Tests {
		// log.Debugf("Checking testcase with prefix %s", testcase.BucketPrefix)
		err := checkTestCase(testcase)
		if err != nil {
			log.WithError(err).Fatalf("Issue detected when scanning through the config file:")
		}
	}
}

func checkTestCase(testcase *TestCaseConfiguration) error {
	if testcase.PayloadGenerator != "" && testcase.PayloadGenerator != "random" && testcase.PayloadGenerator != "empty" {
		return fmt.Errorf("Either random or empty needs to be set for payload generator")
	}
	if testcase.Runtime == 0 && testcase.OpsDeadline == 0 && !testcase.OneShot {
		return fmt.Errorf("Either stop_with_runtime, stop_with_ops or oneshot needs to be set")
	}
	if testcase.ReadWeight == 0 && testcase.WriteWeight == 0 && testcase.ListWeight == 0 && testcase.DeleteWeight == 0 && testcase.ExistingReadWeight == 0 {
		return fmt.Errorf("At least one weight needs to be set - Read / Write / List / Delete")
	}
	if testcase.ExistingReadWeight != 0 && testcase.BucketPrefix == "" {
		return fmt.Errorf("When using existing_read_weight, setting the bucket_prefix is mandatory")
	}
	if testcase.Buckets.NumberMin == 0 {
		return fmt.Errorf("Please set minimum number of Buckets")
	}
	if testcase.Objects.SizeMin == 0 {
		return fmt.Errorf("Please set minimum size of Objects")
	}
	if testcase.Objects.SizeMax == 0 {
		return fmt.Errorf("Please set maximum size of Objects")
	}
	if testcase.Objects.NumberMin == 0 {
		return fmt.Errorf("Please set minimum number of Objects")
	}
	if err := checkDistribution(testcase.Objects.SizeDistribution, "Object size_distribution"); err != nil {
		return err
	}
	if err := checkDistribution(testcase.Objects.NumberDistribution, "Object number_distribution"); err != nil {
		return err
	}
	if err := checkDistribution(testcase.Buckets.NumberDistribution, "Bucket number_distribution"); err != nil {
		return err
	}
	if testcase.Objects.Unit == "" {
		return fmt.Errorf("Please set the Objects unit")
	}

	byteMultiplicator := func(unit string) (int, error) {
		switch strings.ToUpper(unit) {
		case "B":
			return BYTE, nil
		case "KB", "K":
			return KILOBYTE, nil
		case "MB", "M":
			return MEGABYTE, nil
		case "GB", "G":
			return GIGABYTE, nil
		case "TB", "T":
			return TERABYTE, nil
		default:
			return 0, fmt.Errorf("Could not parse unit size - please use one of B/KB/MB/GB/TB")
		}
	}
	multiplicator, err := byteMultiplicator(testcase.Objects.Unit)
	if err != nil {
		return err
	}
	toByteMultiplicator := uint64(multiplicator)

	testcase.Objects.SizeMin = testcase.Objects.SizeMin * toByteMultiplicator
	testcase.Objects.SizeMax = testcase.Objects.SizeMax * toByteMultiplicator
	if testcase.ReadOption != nil {
		readMultiplicator, err := byteMultiplicator(testcase.ReadOption.Unit)
		if err != nil {
			return err
		}
		toReadByteMultiplicator := int64(readMultiplicator)
		testcase.ReadOption.ChunkSize = testcase.ReadOption.ChunkSize * toReadByteMultiplicator
	}
	if testcase.WriteOption != nil {
		writeMultiplicator, err := byteMultiplicator(testcase.WriteOption.Unit)
		if err != nil {
			return err
		}
		toWriteByteMultiplicator := int64(writeMultiplicator)
		testcase.WriteOption.ChunkSize = testcase.WriteOption.ChunkSize * toWriteByteMultiplicator
	}
	return nil
}

// Checks if a given string is of type distribution
func checkDistribution(distribution string, keyname string) error {
	switch distribution {
	case "constant", "random", "sequential":
		return nil
	}
	return fmt.Errorf("%s is not a valid distribution. Allowed options are constant, random, sequential", keyname)
}

// EvaluateDistribution looks at the given distribution and returns a meaningful next number
func EvaluateDistribution(min uint64, max uint64, lastNumber *uint64, increment uint64, distribution string) uint64 {
	switch distribution {
	case "constant":
		return min
	case "random":
		rand.New(rand.NewSource(time.Now().UnixNano()))
		validSize := max - min
		return ((rand.Uint64() % validSize) + min)
	case "sequential":
		*lastNumber = *lastNumber + increment
		if max < *lastNumber || *lastNumber < min {
			*lastNumber = min // rewrap
		}
		return *lastNumber
	}
	return 0
}

// JSON package does not currently marshal/unmarshal time.Duration so we provide a way to do it here
type Duration time.Duration

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	var v interface{}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	switch value := v.(type) {
	case int:
		*d = Duration(time.Duration(value))
	case float64:
		*d = Duration(time.Duration(value))
	case string:
		tmp, err := time.ParseDuration(value)
		if err != nil {
			return err
		}
		*d = Duration(tmp)
	default:
		return errors.New("invalid duration")
	}
	return nil
}

func (d Duration) MarshalYAML() ([]byte, error) {
	return yaml.Marshal(time.Duration(d).String())
}

func (d *Duration) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var v interface{}
	err := unmarshal(&v)
	if err != nil {
		return err
	}
	switch value := v.(type) {
	case int:
		*d = Duration(time.Duration(value))
	case float64:
		*d = Duration(time.Duration(value))
	case string:
		tmp, err := time.ParseDuration(value)
		if err != nil {
			return err
		}
		*d = Duration(tmp)
	default:
		return errors.New("invalid duration")
	}
	return nil
}

var ReadFile = os.ReadFile

func LoadConfigFromFile(configFile string) *TestConf {
	configFileContent, err := ReadFile(configFile)
	if err != nil {
		log.WithError(err).Fatalf("Error reading config file:")
	}
	var config TestConf

	if strings.HasSuffix(configFile, ".yaml") || strings.HasSuffix(configFile, ".yml") {
		err = yaml.Unmarshal(configFileContent, &config)
		if err != nil {
			log.WithError(err).Fatalf("Error unmarshaling yaml config file:")
		}
	} else if strings.HasSuffix(configFile, ".json") {
		err = json.Unmarshal(configFileContent, &config)
		if err != nil {
			log.WithError(err).Fatalf("Error unmarshaling json config file:")
		}
	} else {
		log.WithError(err).Fatalf("Configuration file must be a yaml or json formatted file")
	}

	return &config
}
