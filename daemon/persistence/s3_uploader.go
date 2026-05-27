// SPDX-License-Identifier: Apache-2.0

package persistence

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// S3Uploader implements Uploader by pushing backup files to S3 under a
// per-machine prefix (Workstream B.1). Credentials come from the
// platform-issued IAM user that's already wired through cloud-init
// (AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY env vars on the daemon).
type S3Uploader struct {
	bucket    string
	machineID string
	region    string
	client    *s3.Client
}

// NewS3Uploader builds a client from explicit credentials. Empty values
// fall back to the default credential chain (env, instance profile,
// etc.).
func NewS3Uploader(ctx context.Context, bucket, machineID, region, accessKey, secretKey string) (*S3Uploader, error) {
	if bucket == "" {
		return nil, fmt.Errorf("s3 uploader: bucket required")
	}
	if machineID == "" {
		return nil, fmt.Errorf("s3 uploader: machineID required")
	}
	if region == "" {
		region = "us-east-1"
	}

	var optFns []func(*awsconfig.LoadOptions) error
	optFns = append(optFns, awsconfig.WithRegion(region))
	if accessKey != "" && secretKey != "" {
		optFns = append(optFns, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(accessKey, secretKey, ""),
		))
	}

	cfg, err := awsconfig.LoadDefaultConfig(ctx, optFns...)
	if err != nil {
		return nil, fmt.Errorf("s3 uploader: load aws config: %w", err)
	}

	return &S3Uploader{
		bucket:    bucket,
		machineID: machineID,
		region:    region,
		client:    s3.NewFromConfig(cfg),
	}, nil
}

// Upload pushes a single backup file to s3://<bucket>/<machineId>/<basename>.
// Idempotent: a re-upload of the same path simply creates a new
// version (the bucket has versioning enabled by Pulumi).
func (u *S3Uploader) Upload(ctx context.Context, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("s3 uploader: open %s: %w", path, err)
	}
	defer f.Close()

	key := fmt.Sprintf("%s/%s", u.machineID, filepath.Base(path))
	_, err = u.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(u.bucket),
		Key:    aws.String(key),
		Body:   f,
		// Defense-in-depth: backups are SQLCipher-encrypted at the
		// application layer; SSE-S3 here is bucket-default but we
		// request it explicitly so a misconfigured bucket policy can't
		// downgrade.
		ServerSideEncryption: "AES256",
	})
	if err != nil {
		return fmt.Errorf("s3 uploader: put %s: %w", key, err)
	}
	return nil
}
