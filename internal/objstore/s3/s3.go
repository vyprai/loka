package s3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/vyprai/loka/internal/objstore"
)

// Config configures the S3-compatible object store.
type Config struct {
	Region   string // AWS region (e.g. "us-east-1").
	Endpoint string // Custom endpoint for MinIO, R2, etc. Empty = AWS S3.
}

// Store implements objstore.ObjectStore using AWS S3 or any S3-compatible API.
type Store struct {
	client    *s3.Client
	presigner *s3.PresignClient
}

// New creates a new S3 object store.
func New(ctx context.Context, cfg Config) (*Store, error) {
	var opts []func(*awsconfig.LoadOptions) error
	if cfg.Region != "" {
		opts = append(opts, awsconfig.WithRegion(cfg.Region))
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}

	var s3Opts []func(*s3.Options)
	if cfg.Endpoint != "" {
		s3Opts = append(s3Opts, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
			o.UsePathStyle = true // Required for MinIO / custom endpoints.
		})
	}

	client := s3.NewFromConfig(awsCfg, s3Opts...)
	return &Store{
		client:    client,
		presigner: s3.NewPresignClient(client),
	}, nil
}

func (s *Store) Put(ctx context.Context, bucket, key string, reader io.Reader, size int64) error {
	input := &s3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
		Body:   reader,
	}
	if size >= 0 {
		input.ContentLength = aws.Int64(size)
	}
	_, err := s.client.PutObject(ctx, input)
	if err != nil {
		return fmt.Errorf("s3 put: %w", err)
	}
	return nil
}

func (s *Store) Get(ctx context.Context, bucket, key string) (io.ReadCloser, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("s3 get: %w", err)
	}
	return out.Body, nil
}

func (s *Store) Delete(ctx context.Context, bucket, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("s3 delete: %w", err)
	}
	return nil
}

func (s *Store) Exists(ctx context.Context, bucket, key string) (bool, error) {
	_, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		var notFound *types.NotFound
		var noSuchKey *types.NoSuchKey
		if errors.As(err, &notFound) || errors.As(err, &noSuchKey) {
			return false, nil
		}
		return false, fmt.Errorf("s3 head: %w", err)
	}
	return true, nil
}

func (s *Store) GetPresignedURL(ctx context.Context, bucket, key string, expiry time.Duration) (string, error) {
	out, err := s.presigner.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(expiry))
	if err != nil {
		return "", fmt.Errorf("s3 presign: %w", err)
	}
	return out.URL, nil
}

func (s *Store) List(ctx context.Context, bucket, prefix string) ([]objstore.ObjectInfo, error) {
	var objects []objstore.ObjectInfo
	paginator := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(bucket),
		Prefix: aws.String(prefix),
	})

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("s3 list: %w", err)
		}
		for _, obj := range page.Contents {
			objects = append(objects, objstore.ObjectInfo{
				Key:          aws.ToString(obj.Key),
				Size:         aws.ToInt64(obj.Size),
				LastModified: aws.ToTime(obj.LastModified),
				ETag:         aws.ToString(obj.ETag),
			})
		}
	}
	return objects, nil
}

var _ objstore.ObjectStore = (*Store)(nil)
