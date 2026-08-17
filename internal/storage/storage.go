// Package storage is the object store: S3 in production, MinIO locally. It is
// the only package that knows a bucket exists.
package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/ubilolxon/qrmenu/internal/config"
)

// ErrNotFound is returned for a key that does not exist, so callers can tell
// "the client never uploaded" apart from "the object store is broken".
var ErrNotFound = errors.New("storage: object not found")

type Client struct {
	mc        *minio.Client
	bucket    string
	publicURL string
}

func New(cfg config.Storage) (*Client, error) {
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("storage: S3_ENDPOINT is empty")
	}

	mc, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.UseSSL,
		Region: cfg.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("storage: %w", err)
	}

	return &Client{
		mc:        mc,
		bucket:    cfg.Bucket,
		publicURL: strings.TrimRight(cfg.PublicURL, "/"),
	}, nil
}

func (c *Client) Bucket() string { return c.bucket }

// PublicURL is where a browser fetches a key from. In production this is the
// CDN in front of the bucket, which is why it is configured separately from the
// endpoint the server uploads through.
func (c *Client) PublicURL(key string) string {
	return c.publicURL + "/" + key
}

// EnsureBucket creates the bucket when it is missing.
//
// Called at startup so a fresh local checkout works without a console visit. In
// production the bucket and its policy are provisioned outside the application,
// and the credentials there are unlikely to permit CreateBucket at all — hence
// the failure is a warning to the caller, not a fatal error.
func (c *Client) EnsureBucket(ctx context.Context, region string) error {
	exists, err := c.mc.BucketExists(ctx, c.bucket)
	if err != nil {
		return fmt.Errorf("storage: check bucket: %w", err)
	}
	if exists {
		return nil
	}
	if err := c.mc.MakeBucket(ctx, c.bucket, minio.MakeBucketOptions{Region: region}); err != nil {
		return fmt.Errorf("storage: create bucket %q: %w", c.bucket, err)
	}

	// A bucket the application just created but cannot serve from is useless:
	// every image would 403. So a *newly created* bucket is made readable,
	// matching what deploy/docker-compose.yml provisions and what a CDN-fronted
	// bucket looks like in production.
	//
	// This deliberately never touches an existing bucket. Widening the policy on
	// a bucket someone else provisioned would be a security change made behind
	// their back.
	if err := c.mc.SetBucketPolicy(ctx, c.bucket, readOnlyPolicy(c.bucket)); err != nil {
		return fmt.Errorf("storage: make bucket %q readable: %w", c.bucket, err)
	}
	return nil
}

// readOnlyPolicy allows anonymous GET and nothing else. Listing is excluded on
// purpose: object keys contain restaurant ids, and a listable bucket would let
// anyone enumerate every image on the platform.
func readOnlyPolicy(bucket string) string {
	return `{"Version":"2012-10-17","Statement":[{` +
		`"Effect":"Allow","Principal":{"AWS":["*"]},` +
		`"Action":["s3:GetObject"],` +
		`"Resource":["arn:aws:s3:::` + bucket + `/*"]}]}`
}

// PresignPut returns a URL the client can PUT bytes to directly.
//
// This is the whole point of the upload design: image bytes never travel
// through the API. A restaurant uploading fifty dish photos would otherwise
// occupy an API worker for the duration of each one, on connections that are
// slow precisely because they are phones.
//
// The content type is part of the signature, so a client cannot presign for a
// JPEG and then declare something else on the PUT. That is a useful constraint
// but not a security control — the declared type still says nothing about the
// actual bytes, which is why every upload is decoded before it is published.
func (c *Client) PresignPut(ctx context.Context, key, contentType string, expiry time.Duration) (string, error) {
	headers := http.Header{}
	headers.Set("Content-Type", contentType)

	u, err := c.mc.PresignHeader(ctx, http.MethodPut, c.bucket, key, expiry, url.Values{}, headers)
	if err != nil {
		return "", fmt.Errorf("storage: presign put %q: %w", key, err)
	}
	return u.String(), nil
}

type ObjectInfo struct {
	Size        int64
	ContentType string
}

func (c *Client) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	info, err := c.mc.StatObject(ctx, c.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		return ObjectInfo{}, mapErr(key, err)
	}
	return ObjectInfo{Size: info.Size, ContentType: info.ContentType}, nil
}

// Get opens an object for reading. The caller closes it.
func (c *Client) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	obj, err := c.mc.GetObject(ctx, c.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, mapErr(key, err)
	}
	// GetObject is lazy — it does not talk to the server until the first read,
	// so a missing key would otherwise only surface later, far from here.
	if _, err := obj.Stat(); err != nil {
		_ = obj.Close()
		return nil, mapErr(key, err)
	}
	return obj, nil
}

// Put writes an object. Derivatives are immutable and addressed by a key that
// contains the image id, so they are marked cacheable forever.
func (c *Client) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	_, err := c.mc.PutObject(ctx, c.bucket, key, r, size, minio.PutObjectOptions{
		ContentType:  contentType,
		CacheControl: "public, max-age=31536000, immutable",
	})
	if err != nil {
		return fmt.Errorf("storage: put %q: %w", key, err)
	}
	return nil
}

// RemovePrefix deletes every object under a prefix.
//
// Deleting an image means deleting the original and all its derivatives. Doing
// that by prefix rather than by listing known keys means a derivative added in
// a later version, or one written by a run that failed halfway, still gets
// cleaned up instead of being orphaned and paid for forever.
func (c *Client) RemovePrefix(ctx context.Context, prefix string) error {
	if prefix == "" || !strings.HasSuffix(prefix, "/") {
		// A prefix without a trailing slash would also match sibling keys that
		// merely start with the same characters.
		return fmt.Errorf("storage: refusing to remove non-directory prefix %q", prefix)
	}

	objects := c.mc.ListObjects(ctx, c.bucket, minio.ListObjectsOptions{
		Prefix:    prefix,
		Recursive: true,
	})

	var firstErr error
	for err := range c.mc.RemoveObjects(ctx, c.bucket, keysOf(objects), minio.RemoveObjectsOptions{}) {
		if firstErr == nil {
			firstErr = fmt.Errorf("storage: remove %q: %w", err.ObjectName, err.Err)
		}
	}
	return firstErr
}

// keysOf adapts a listing channel to the removal channel, dropping listing
// errors — a key that could not be listed cannot be deleted either, and the
// sweep will see it again next time.
func keysOf(in <-chan minio.ObjectInfo) <-chan minio.ObjectInfo {
	out := make(chan minio.ObjectInfo)
	go func() {
		defer close(out)
		for obj := range in {
			if obj.Err != nil {
				continue
			}
			out <- obj
		}
	}()
	return out
}

func mapErr(key string, err error) error {
	if minio.ToErrorResponse(err).Code == "NoSuchKey" {
		return fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	return fmt.Errorf("storage: %q: %w", key, err)
}
