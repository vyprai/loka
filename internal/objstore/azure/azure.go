package azure

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/sas"

	"github.com/vyprai/loka/internal/objstore"
)

// Config configures the Azure Blob Storage object store.
type Config struct {
	Account string // Storage account name.
}

// Store implements objstore.ObjectStore using Azure Blob Storage.
// "Buckets" map to Azure containers, "keys" map to blob names.
type Store struct {
	client  *azblob.Client
	account string
}

// New creates a new Azure Blob Storage object store.
// Uses DefaultAzureCredential (env vars, managed identity, CLI, etc).
func New(ctx context.Context, cfg Config) (*Store, error) {
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("azure credential: %w", err)
	}

	serviceURL := fmt.Sprintf("https://%s.blob.core.windows.net/", cfg.Account)
	client, err := azblob.NewClient(serviceURL, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("create azure client: %w", err)
	}

	return &Store{client: client, account: cfg.Account}, nil
}

func (s *Store) Put(ctx context.Context, bucket, key string, reader io.Reader, _ int64) error {
	data, err := io.ReadAll(reader)
	if err != nil {
		return fmt.Errorf("read data: %w", err)
	}
	_, err = s.client.UploadBuffer(ctx, bucket, key, data, nil)
	if err != nil {
		return fmt.Errorf("azure upload: %w", err)
	}
	return nil
}

func (s *Store) Get(ctx context.Context, bucket, key string) (io.ReadCloser, error) {
	resp, err := s.client.DownloadStream(ctx, bucket, key, nil)
	if err != nil {
		return nil, fmt.Errorf("azure download: %w", err)
	}
	return resp.Body, nil
}

func (s *Store) Delete(ctx context.Context, bucket, key string) error {
	_, err := s.client.DeleteBlob(ctx, bucket, key, nil)
	if err != nil {
		return fmt.Errorf("azure delete: %w", err)
	}
	return nil
}

func (s *Store) Exists(ctx context.Context, bucket, key string) (bool, error) {
	_, err := s.client.ServiceClient().NewContainerClient(bucket).NewBlobClient(key).GetProperties(ctx, nil)
	if err != nil {
		if isBlobNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("azure head: %w", err)
	}
	return true, nil
}

func (s *Store) GetPresignedURL(ctx context.Context, bucket, key string, expiry time.Duration) (string, error) {
	blobClient := s.client.ServiceClient().NewContainerClient(bucket).NewBlobClient(key)
	permissions := sas.BlobPermissions{Read: true}
	sasURL, err := blobClient.GetSASURL(permissions, time.Now().Add(expiry), &blob.GetSASURLOptions{})
	if err != nil {
		return "", fmt.Errorf("azure sas url: %w", err)
	}
	return sasURL, nil
}

func (s *Store) List(ctx context.Context, bucket, prefix string) ([]objstore.ObjectInfo, error) {
	var objects []objstore.ObjectInfo
	pager := s.client.NewListBlobsFlatPager(bucket, &azblob.ListBlobsFlatOptions{
		Prefix: &prefix,
	})
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("azure list: %w", err)
		}
		for _, item := range page.Segment.BlobItems {
			info := objstore.ObjectInfo{
				Key: *item.Name,
			}
			if item.Properties != nil {
				if item.Properties.ContentLength != nil {
					info.Size = *item.Properties.ContentLength
				}
				if item.Properties.LastModified != nil {
					info.LastModified = *item.Properties.LastModified
				}
				if item.Properties.ETag != nil {
					info.ETag = string(*item.Properties.ETag)
				}
			}
			objects = append(objects, info)
		}
	}
	return objects, nil
}

// isBlobNotFound checks if the error indicates a blob was not found.
func isBlobNotFound(err error) bool {
	var respErr *azcore.ResponseError
	if errors.As(err, &respErr) {
		return respErr.StatusCode == 404
	}
	return false
}

var _ objstore.ObjectStore = (*Store)(nil)
