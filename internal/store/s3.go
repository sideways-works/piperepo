package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// S3Config says how to reach a bucket.
type S3Config struct {
	Endpoint  string
	Region    string
	Bucket    string
	AccessKey string
	SecretKey string
	PathStyle bool
}

// S3 is a store in one S3 bucket.
type S3 struct {
	client *s3.Client
	bucket string
}

// NewS3 is a store in c's bucket.
func NewS3(c S3Config) *S3 {
	region := c.Region
	if region == "" {
		region = "us-east-1"
	}

	opts := s3.Options{
		Region:       region,
		Credentials:  credentials.NewStaticCredentialsProvider(c.AccessKey, c.SecretKey, ""),
		UsePathStyle: c.PathStyle,
		HTTPClient:   &http.Client{Timeout: 10 * time.Minute},
		// Any S3 takes these; checksums some don't.
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	}
	if c.Endpoint != "" {
		opts.BaseEndpoint = aws.String(c.Endpoint)
	}

	return &S3{client: s3.New(opts), bucket: c.Bucket}
}

func notFound(err error) bool {
	if _, ok := errors.AsType[*types.NoSuchKey](err); ok {
		return true
	}

	if _, ok := errors.AsType[*types.NotFound](err); ok {
		return true
	}

	var api smithy.APIError

	return errors.As(err, &api) && (api.ErrorCode() == "NotFound" || api.ErrorCode() == "NoSuchKey")
}

func (s *S3) Get(ctx context.Context, key string) (io.ReadCloser, Info, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: &s.bucket, Key: &key})
	if err != nil {
		if notFound(err) {
			return nil, Info{}, ErrNotFound
		}

		return nil, Info{}, fmt.Errorf("s3: get %s: %w", key, err)
	}

	return out.Body, Info{Key: key, Size: aws.ToInt64(out.ContentLength), ETag: strings.Trim(aws.ToString(out.ETag), `"`), Modified: aws.ToTime(out.LastModified)}, nil
}

func (s *S3) Put(ctx context.Context, key string, body io.Reader, size int64) (string, error) {
	out, err := s.client.PutObject(ctx, &s3.PutObjectInput{Bucket: &s.bucket, Key: &key, Body: body, ContentLength: aws.Int64(size)})
	if err != nil {
		return "", fmt.Errorf("s3: put %s: %w", key, err)
	}

	return strings.Trim(aws.ToString(out.ETag), `"`), nil
}

func (s *S3) Head(ctx context.Context, key string) (Info, error) {
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &s.bucket, Key: &key})
	if err != nil {
		if notFound(err) {
			return Info{}, ErrNotFound
		}

		return Info{}, fmt.Errorf("s3: head %s: %w", key, err)
	}

	return Info{Key: key, Size: aws.ToInt64(out.ContentLength), ETag: strings.Trim(aws.ToString(out.ETag), `"`), Modified: aws.ToTime(out.LastModified)}, nil
}

func (s *S3) List(ctx context.Context, prefix string) ([]Info, error) {
	var out []Info

	p := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{Bucket: &s.bucket, Prefix: &prefix})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("s3: list %s: %w", prefix, err)
		}

		for _, o := range page.Contents {
			out = append(out, Info{Key: aws.ToString(o.Key), Size: aws.ToInt64(o.Size), ETag: strings.Trim(aws.ToString(o.ETag), `"`), Modified: aws.ToTime(o.LastModified)})
		}
	}

	return out, nil
}

func (s *S3) Prefixes(ctx context.Context, prefix string) ([]string, error) {
	var out []string

	p := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{Bucket: &s.bucket, Prefix: &prefix, Delimiter: aws.String("/")})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("s3: list %s: %w", prefix, err)
		}

		for _, c := range page.CommonPrefixes {
			out = append(out, aws.ToString(c.Prefix))
		}
	}

	return out, nil
}

func (s *S3) Delete(ctx context.Context, key string) error {
	if _, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &s.bucket, Key: &key}); err != nil && !notFound(err) {
		return fmt.Errorf("s3: delete %s: %w", key, err)
	}

	return nil
}
