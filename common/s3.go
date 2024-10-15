package common

import (
	"context"
	"crypto/tls"
	"net/http"

	"github.com/aws/aws-sdk-go-v2/aws"
	s3config "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	log "github.com/sirupsen/logrus"
	"go.opencensus.io/plugin/ochttp"
)

func NewS3Client(ctx context.Context, endpoint, accessKey, secretKey string, region string, skipSSLVerify, skipStats bool) (*s3.Client, error) {
	// All clients require a Session. The Session provides the client with
	// shared configuration such as region, endpoint, and credentials. A
	// Session should be shared where possible to take advantage of
	// configuration and credential caching. See the session package for
	// more information.
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: skipSSLVerify},
	}
	var hc *http.Client
	if skipStats {
		// Use this Session to do things that are hidden from the performance monitoring
		// Setting up the housekeeping S3 client
		hc = &http.Client{Transport: tr}

	} else {
		hc = &http.Client{Transport: &ochttp.Transport{Base: tr}}
	}

	cfg, err := s3config.LoadDefaultConfig(ctx,
		s3config.WithHTTPClient(hc),
		s3config.WithRegion(region),
		s3config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")),
	)
	if err != nil {
		log.WithError(err).Warningf("Unable to build S3 config")
		return nil, err
	}

	// Create a new instance of the service's client with a Session.
	// Optional aws.Config values can also be provided as variadic arguments
	// to the New function. This option allows you to provide service
	// specific configuration.
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
	})
	return client, nil
}
