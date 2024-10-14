package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	s3manager "github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	log "github.com/sirupsen/logrus"

	"github.com/mulbc/gosbench/common"
)

var (
	svc, housekeepingSvc *s3.Client
	ctx                  context.Context
	hc                   *http.Client
)

// initS3 initialises the S3 session
func (w *Worker) initS3() {
	id := w.config.ID

	gatewayName := os.Getenv("GATEWAY_NAME")
	if gatewayName == "" {
		gatewayName, _ = os.Hostname()
	}
	if gatewayName != "" {
		// prefer to select s3 gateway of the host machine if enable client and gateway colocation
		for i, conf := range w.config.S3Configs {
			if conf.Name == gatewayName {
				if w.config.ClientGatewayColocation {
					id = i
				} else {
					id = i + 1
				}
				break
			}
		}
	}
	config := w.config.S3Configs[id%len(w.config.S3Configs)]

	log.Infof("s3 config info for worker %s: %+v", w.config.WorkerID, config)

	// TODO Create a context with a timeout - we already use this context in all S3 calls
	// Usually this shouldn't be a problem ;)
	ctx = context.Background()

	var err error

	// Create a new instance of the service's client with a Session.
	// Optional aws.Config values can also be provided as variadic arguments
	// to the New function. This option allows you to provide service
	// specific configuration.
	svc, err = common.NewS3Client(ctx, config.Endpoint, config.AccessKey, config.SecretKey, config.Region, config.SkipSSLVerify, false)
	if err != nil {
		log.WithError(err).Fatal("Unable to build S3 config")
	}

	// Use this service to do things that are hidden from the performance monitoring
	housekeepingSvc, err = common.NewS3Client(ctx, config.Endpoint, config.AccessKey, config.SecretKey, config.Region, config.SkipSSLVerify, true)
	if err != nil {
		log.WithError(err).Fatal("Unable to build S3 config")
	}

	log.Info("S3 Init done")
}

func putObject(service *s3.Client, conf *common.TestCaseConfiguration, op *BaseOperation) (err error) {
	bucket := op.Bucket
	objectName := op.ObjectName
	objectContent := bytes.NewReader(generateBytes(conf.PayloadGenerator, op.TestName, op.ObjectSize))
	start := time.Now()
	if conf.WriteOption != nil {
		// https://aws.github.io/aws-sdk-go-v2/docs/sdk-utilities/s3/
		// Create an uploader with S3 client and custom options
		uploader := s3manager.NewUploader(service)

		_, err = uploader.Upload(ctx, &s3.PutObjectInput{
			Bucket: &bucket,
			Key:    &objectName,
			Body:   objectContent,
		}, func(d *s3manager.Uploader) {
			d.MaxUploadParts = conf.WriteOption.MaxUploadParts
			d.Concurrency = conf.WriteOption.Concurrency
			d.PartSize = conf.WriteOption.ChunkSize
		})
	} else {
		_, err = service.PutObject(ctx, &s3.PutObjectInput{
			Bucket: &bucket,
			Key:    &objectName,
			Body:   objectContent,
		})
	}
	if err != nil {
		log.WithError(err).WithField("object", objectName).WithField("bucket", bucket).Errorf("Failed to upload object")
		return err
	}
	duration := time.Since(start)
	promLatency.WithLabelValues(op.TestName, "PUT").Observe(float64(duration.Milliseconds()))

	log.WithField("bucket", bucket).WithField("key", objectName).Tracef("Upload successful")

	return err
}

func listObjects(service *s3.Client, prefix string, bucket string) ([]types.Object, error) {
	var bucketContents []types.Object
	p := s3.NewListObjectsV2Paginator(service, &s3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String(prefix)})
	for p.HasMorePages() {
		// Next Page takes a new context for each page retrieval. This is where
		// you could add timeouts or deadlines.
		page, err := p.NextPage(ctx)
		if err != nil {
			log.WithError(err).WithField("prefix", prefix).WithField("bucket", bucket).Errorf("Failed to list objects")
			return nil, err
		}
		bucketContents = append(bucketContents, page.Contents...)
	}

	return bucketContents, nil
}

func headObject(service *s3.Client, objectName string, bucket string) (*s3.HeadObjectOutput, error) {
	result, err := service.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: &bucket,
		Key:    &objectName,
	})
	if err != nil {
		// Cast err to awserr.Error to handle specific error codes.
		// https://github.com/aws/aws-sdk-go-v2/issues/2084
		// https://github.com/aws/aws-sdk-go-v2/issues/1110
		var oe smithy.APIError
		if errors.As(err, &oe) && oe.ErrorCode() == "NotFound" {
			log.WithError(oe).Errorf("Could not find prefix %s in bucket %s when querying properties", objectName, bucket)
		}
	}
	return result, err
}

type discarder struct {
}

func (d *discarder) WriteAt(p []byte, off int64) (n int, err error) {
	return len(p), nil
}

func getObject(service *s3.Client, conf *common.TestCaseConfiguration, op *BaseOperation) (err error) {
	var (
		bucket     = op.Bucket
		objectName = op.ObjectName
		objectSize = op.ObjectSize
		numBytes   int64
	)
	if conf.ReadOption != nil {
		// Create a downloader with the session and custom options
		downloader := s3manager.NewDownloader(service)
		_, err = downloader.Download(ctx, &discarder{}, &s3.GetObjectInput{
			Bucket: aws.String(bucket),
			Key:    &objectName,
		}, func(d *s3manager.Downloader) {
			d.Concurrency = conf.ReadOption.Concurrency
			d.PartSize = conf.ReadOption.ChunkSize
		})
	} else {
		// Remove the allocation of buffer
		result, err := svc.GetObject(ctx, &s3.GetObjectInput{
			Bucket: &bucket,
			Key:    &objectName,
		})
		if err != nil {
			return err
		}
		start := time.Now()
		numBytes, err = io.Copy(io.Discard, result.Body)
		if err != nil {
			return err
		}
		duration := time.Since(start)
		promIOCopyLatency.WithLabelValues(op.TestName).Observe(float64(duration.Milliseconds()))
	}
	if numBytes != int64(objectSize) {
		return fmt.Errorf("Expected object length %d is not matched to actual object length %d", objectSize, numBytes)
	}
	return nil
}

func deleteObject(service *s3.Client, objectName string, bucket string) error {
	_, err := service.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: &bucket,
		Key:    &objectName,
	})
	if err != nil {
		log.WithError(err).Errorf("Could not find object %s in bucket %s for deletion", objectName, bucket)
	}
	return err
}

func createBucket(service *s3.Client, bucket string) error {
	// Do not err when the bucket is already there...
	_, err := service.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: &bucket,
	})
	if err != nil {
		var bne *types.BucketAlreadyExists
		// Ignore error if bucket already exists
		if errors.As(err, &bne) {
			return nil
		}
		log.WithError(err).Errorf("Issues when creating bucket %s", bucket)
	}
	return err
}

func deleteBucket(service *s3.Client, bucket string) error {
	// First delete all objects in the bucket
	input := &s3.ListObjectsV2Input{
		Bucket: aws.String(bucket),
	}

	var bucketContents []types.Object
	isTruncated := true
	for isTruncated {
		result, err := service.ListObjectsV2(ctx, input)
		if err != nil {
			return err
		}
		bucketContents = append(bucketContents, result.Contents...)
		input.ContinuationToken = result.NextContinuationToken
		isTruncated = *result.IsTruncated
	}

	if len(bucketContents) > 0 {
		var objectsToDelete []types.ObjectIdentifier
		for _, item := range bucketContents {
			objectsToDelete = append(objectsToDelete, types.ObjectIdentifier{
				Key: item.Key,
			})
		}

		deleteObjectsInput := &s3.DeleteObjectsInput{
			Bucket: aws.String(bucket),
			Delete: &types.Delete{
				Objects: objectsToDelete,
				Quiet:   aws.Bool(true),
			},
		}

		_, err := svc.DeleteObjects(ctx, deleteObjectsInput)
		if err != nil {
			return err
		}
	}

	// Then delete the (now empty) bucket itself
	_, err := service.DeleteBucket(ctx, &s3.DeleteBucketInput{
		Bucket: &bucket,
	})
	return err
}
