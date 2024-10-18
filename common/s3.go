package common

import (
	"context"
	"crypto/tls"
	"net/http"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/ratelimit"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
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
		// dial tcp 10.3.9.232:80: connect: cannot assign requested address
		// https://github.com/golang/go/issues/16012
		MaxIdleConnsPerHost: 100,
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
		s3config.WithRetryer(func() aws.Retryer {
			return retry.NewStandard(func(o *retry.StandardOptions) {
				// In high concurrency scenarios, the AWS SDK experiences a large number of errors of
				// "failed to get rate limit token, retry quota exceeded, 0 available, 5 requested"
				// https://aws.github.io/aws-sdk-go-v2/docs/configuring-sdk/retries-timeouts/#client-side-rate-limiting
				o.RateLimiter = ratelimit.None
			})
		}),
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
