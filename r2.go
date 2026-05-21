package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

type r2Client struct {
	client        *s3.Client
	bucket        string
	publicBaseURL string
}

type r2Config struct {
	accountID       string
	accessKeyID     string
	secretAccessKey string
	bucket          string
	publicBaseURL   string
}

func loadR2ConfigFromEnv() (r2Config, error) {
	cfg := r2Config{
		accountID:       strings.TrimSpace(os.Getenv("R2_ACCOUNT_ID")),
		accessKeyID:     strings.TrimSpace(os.Getenv("R2_ACCESS_KEY_ID")),
		secretAccessKey: strings.TrimSpace(os.Getenv("R2_SECRET_ACCESS_KEY")),
		bucket:          strings.TrimSpace(os.Getenv("R2_BUCKET")),
		publicBaseURL:   strings.TrimRight(strings.TrimSpace(os.Getenv("R2_PUBLIC_BASE_URL")), "/"),
	}
	missing := []string{}
	if cfg.accountID == "" {
		missing = append(missing, "R2_ACCOUNT_ID")
	}
	if cfg.accessKeyID == "" {
		missing = append(missing, "R2_ACCESS_KEY_ID")
	}
	if cfg.secretAccessKey == "" {
		missing = append(missing, "R2_SECRET_ACCESS_KEY")
	}
	if cfg.bucket == "" {
		missing = append(missing, "R2_BUCKET")
	}
	if cfg.publicBaseURL == "" {
		missing = append(missing, "R2_PUBLIC_BASE_URL")
	}
	if len(missing) > 0 {
		return cfg, fmt.Errorf("missing required env vars: %s", strings.Join(missing, ", "))
	}
	return cfg, nil
}

func newR2Client(ctx context.Context, cfg r2Config) (*r2Client, error) {
	endpoint := fmt.Sprintf("https://%s.r2.cloudflarestorage.com", cfg.accountID)

	awsCfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion("auto"),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(cfg.accessKeyID, cfg.secretAccessKey, "")),
	)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}

	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
	})

	return &r2Client{
		client:        client,
		bucket:        cfg.bucket,
		publicBaseURL: cfg.publicBaseURL,
	}, nil
}

func (c *r2Client) put(ctx context.Context, key, contentType string, body io.Reader, size int64) error {
	_, err := c.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(c.bucket),
		Key:           aws.String(key),
		Body:          body,
		ContentLength: aws.Int64(size),
		ContentType:   aws.String(contentType),
	})
	return err
}

func (c *r2Client) get(ctx context.Context, key string, rangeHeader string) (*s3.GetObjectOutput, error) {
	in := &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	}
	if rangeHeader != "" {
		in.Range = aws.String(rangeHeader)
	}
	return c.client.GetObject(ctx, in)
}

func (c *r2Client) delete(ctx context.Context, key string) error {
	_, err := c.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	return err
}

func (c *r2Client) head(ctx context.Context, key string) (*s3.HeadObjectOutput, error) {
	return c.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
}

func (c *r2Client) publicURL(key string) string {
	return c.publicBaseURL + "/" + key
}

func (c *r2Client) presignedGetURL(ctx context.Context, key string, ttl time.Duration) (string, error) {
	presigner := s3.NewPresignClient(c.client)
	req, err := presigner.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", err
	}
	return req.URL, nil
}

func isR2NotFound(err error) bool {
	var nsk *types.NoSuchKey
	if errors.As(err, &nsk) {
		return true
	}
	var nf *types.NotFound
	if errors.As(err, &nf) {
		return true
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		code := apiErr.ErrorCode()
		if code == "NoSuchKey" || code == "NotFound" || code == "404" {
			return true
		}
	}
	return false
}

// proxyR2Object streams an object from R2 to the client, forwarding Range
// headers and propagating relevant response headers (Content-Type, Content-Length,
// Content-Range, Accept-Ranges, ETag, Last-Modified).
func (c *r2Client) proxyObject(ctx context.Context, w http.ResponseWriter, r *http.Request, key string, cacheFor time.Duration) {
	out, err := c.get(ctx, key, r.Header.Get("Range"))
	if err != nil {
		if isR2NotFound(err) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	defer out.Body.Close()

	if out.ContentType != nil {
		w.Header().Set("Content-Type", *out.ContentType)
	}
	if out.ContentLength != nil {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", *out.ContentLength))
	}
	if out.ContentRange != nil {
		w.Header().Set("Content-Range", *out.ContentRange)
	}
	if out.AcceptRanges != nil {
		w.Header().Set("Accept-Ranges", *out.AcceptRanges)
	} else {
		w.Header().Set("Accept-Ranges", "bytes")
	}
	if out.ETag != nil {
		w.Header().Set("ETag", *out.ETag)
	}
	if out.LastModified != nil {
		w.Header().Set("Last-Modified", out.LastModified.UTC().Format(http.TimeFormat))
	}
	if cacheFor > 0 {
		w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", int(cacheFor.Seconds())))
	}

	status := http.StatusOK
	if r.Header.Get("Range") != "" && out.ContentRange != nil {
		status = http.StatusPartialContent
	}
	w.WriteHeader(status)
	_, _ = io.Copy(w, out.Body)
}
