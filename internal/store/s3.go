package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// S3 stores objects in a bucket, optionally beneath a prefix.
type S3 struct {
	client *s3.Client
	bucket string
	prefix string
}

// NewS3 opens a store from s3://bucket/prefix?endpoint=...&region=...
func NewS3(uri *url.URL) (*S3, error) {
	if uri.Host == "" {
		return nil, errors.New("s3 uri has no bucket")
	}
	key, secret := os.Getenv("JELLYSCALE_S3_ACCESS_KEY"), os.Getenv("JELLYSCALE_S3_SECRET_KEY")
	if key == "" || secret == "" {
		return nil, errors.New("JELLYSCALE_S3_ACCESS_KEY and JELLYSCALE_S3_SECRET_KEY are required for s3 stores")
	}
	region := uri.Query().Get("region")
	if region == "" {
		region = "us-east-1"
	}
	options := s3.Options{
		Region:      region,
		Credentials: credentials.NewStaticCredentialsProvider(key, secret, ""),
	}
	if endpoint := uri.Query().Get("endpoint"); endpoint != "" {
		options.BaseEndpoint = &endpoint
		options.UsePathStyle = true
	}
	return &S3{client: s3.New(options), bucket: uri.Host, prefix: strings.Trim(uri.Path, "/")}, nil
}

func (s *S3) objectKey(key string) string {
	if s.prefix == "" {
		return key
	}
	return s.prefix + "/" + key
}

// List pages through the bucket. Each listing page already carries size and
// ETag, so a scan costs one request per thousand objects, with no HEADs.
func (s *S3) List(ctx context.Context, prefix string) ([]Object, error) {
	root := s.objectKey(prefix)
	if root != "" && !strings.HasSuffix(root, "/") {
		root += "/"
	}
	var objects []Object
	pages := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{Bucket: &s.bucket, Prefix: &root})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", s.bucket, err)
		}
		for _, item := range page.Contents {
			if strings.HasSuffix(*item.Key, "/") {
				continue
			}
			objects = append(objects, Object{
				Key:  strings.TrimPrefix(*item.Key, s.objectKey("")),
				Size: aws.ToInt64(item.Size),
				ETag: strings.Trim(aws.ToString(item.ETag), `"`),
			})
		}
	}
	return objects, nil
}

// Open reads from offset to the end of the object in one GET.
func (s *S3) Open(ctx context.Context, key string, offset int64) (io.ReadCloser, error) {
	objectKey := s.objectKey(key)
	input := &s3.GetObjectInput{Bucket: &s.bucket, Key: &objectKey}
	if offset > 0 {
		input.Range = aws.String(fmt.Sprintf("bytes=%d-", offset))
	}
	output, err := s.client.GetObject(ctx, input)
	if err != nil {
		var noKey *types.NoSuchKey
		var api smithy.APIError
		if errors.As(err, &noKey) || (errors.As(err, &api) && api.ErrorCode() == "NotFound") {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get %s: %w", key, err)
	}
	if input.Range != nil && output.ContentRange == nil { // Not the range asked for.
		output.Body.Close()
		return &whole{ReadCloser: http.NoBody, left: 1}, nil
	}
	return &whole{ReadCloser: output.Body, left: aws.ToInt64(output.ContentLength)}, nil
}

// whole turns a body that ends before its Content-Length into an error, which
// SeaweedFS's cut GETs would not otherwise give.
type whole struct {
	io.ReadCloser
	left int64
}

func (w *whole) Read(p []byte) (int, error) {
	n, err := w.ReadCloser.Read(p)
	w.left -= int64(n)
	if err == io.EOF && w.left > 0 {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}

// Put buffers the body, because signing needs a seekable payload. Bodies are
// images and segments, not media.
func (s *S3) Put(ctx context.Context, key string, body io.Reader) error {
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	objectKey := s.objectKey(key)
	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{Bucket: &s.bucket, Key: &objectKey, Body: bytes.NewReader(data)})
	if err != nil {
		return fmt.Errorf("put %s: %w", key, err)
	}
	return nil
}

// Delete removes an object. S3 answers success for a key that isn't there.
func (s *S3) Delete(ctx context.Context, key string) error {
	objectKey := s.objectKey(key)
	if _, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &s.bucket, Key: &objectKey}); err != nil {
		return fmt.Errorf("delete %s: %w", key, err)
	}
	return nil
}

// LocalPath is always false: objects have no filesystem path.
func (s *S3) LocalPath(string) (string, bool) {
	return "", false
}
