// Command seed copies the films and the show of the fixtures image into an S3 bucket, for the S3 libraries of
// deploy/fixtures/compose.yaml. The image holds each file once, so each copy is renamed, with " S3" after its title and
// in its files' names: the server's names stay unique. A copy that the bucket already has at its size is kept, so the
// seed service of a second `up` copies nothing. Only the fixtures image holds it.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"jellyscale/internal/store"
)

// copyOf is one file of a copy: from is its path under the media root, to its key in the bucket.
type copyOf struct{ from, to string }

// copies are the S3 libraries' files: the films the criteria of test/image.sh name, and the show Delta Show with its
// three episodes, each copy under its own name.
var copies = []copyOf{
	{"movies/Alpha Film (2021)/Alpha Film (2021).mkv", "media/Alpha Film S3 (2021)/Alpha Film S3 (2021).mkv"},
	{"transcode/Tau Film (2035)/Tau Film (2035).mkv", "media/Tau Film S3 (2035)/Tau Film S3 (2035).mkv"},
	{"transcode/Pgs Hevc Film (2090)/Pgs Hevc Film (2090).mkv", "media/Pgs Hevc Film S3 (2090)/Pgs Hevc Film S3 (2090).mkv"},
	{"transcode/Resh Film (2085)/Resh Film (2085).mkv", "media/Resh Film S3 (2085)/Resh Film S3 (2085).mkv"},
	{"languages/Babel Film/Babel Film.mkv", "media/Babel Film S3/Babel Film S3.mkv"},
	{"languages/Babel Film/Babel Film.de.srt", "media/Babel Film S3/Babel Film S3.de.srt"},
	{"languages/Babel Film/Babel Film.es.srt", "media/Babel Film S3/Babel Film S3.es.srt"},
	{"shows/Delta Show (2020)/Season 01/Delta Show S01E01 Pilot.mkv", "shows/Delta Show S3 (2020)/Season 01/Delta Show S3 S01E01 Pilot S3.mkv"},
	{"shows/Delta Show (2020)/Season 01/Delta Show S01E02 Second.mkv", "shows/Delta Show S3 (2020)/Season 01/Delta Show S3 S01E02 Second S3.mkv"},
	{"shows/Delta Show (2020)/Season 02/Delta Show S02E01 Return.mkv", "shows/Delta Show S3 (2020)/Season 02/Delta Show S3 S02E01 Return S3.mkv"},
}

func main() {
	endpoint := flag.String("endpoint", "http://seaweedfs:8333", "the S3 endpoint")
	bucket := flag.String("bucket", "jellyscale-fixtures", "the bucket the copies go to")
	root := flag.String("root", "/fixtures/media", "the media root the copies are read from")
	flag.Parse()
	if err := run(*endpoint, *bucket, *root); err != nil {
		log.Fatal(err)
	}
}

func run(endpoint, bucket, root string) error {
	ctx := context.Background()
	if err := createBucket(ctx, endpoint, bucket); err != nil {
		return err
	}
	out, err := store.Open(fmt.Sprintf("s3://%s?endpoint=%s", bucket, endpoint))
	if err != nil {
		return err
	}
	for _, copied := range copies {
		if err := copyFile(ctx, out, filepath.Join(root, copied.from), copied.to); err != nil {
			return fmt.Errorf("copying %s: %w", copied.from, err)
		}
	}
	return nil
}

// createBucket retries until the endpoint answers: the seed service starts with SeaweedFS, which may still be starting.
func createBucket(ctx context.Context, endpoint, bucket string) error {
	client := s3.New(s3.Options{
		Region:       "us-east-1",
		Credentials:  credentials.NewStaticCredentialsProvider(os.Getenv("JELLYSCALE_S3_ACCESS_KEY"), os.Getenv("JELLYSCALE_S3_SECRET_KEY"), ""),
		BaseEndpoint: aws.String(endpoint),
		UsePathStyle: true,
	})
	deadline := time.Now().Add(2 * time.Minute)
	for {
		_, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)})
		var owned *types.BucketAlreadyOwnedByYou
		var exists *types.BucketAlreadyExists
		if err == nil || errors.As(err, &owned) || errors.As(err, &exists) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("creating bucket %s at %s: %w", bucket, endpoint, err)
		}
		time.Sleep(time.Second)
	}
}

// copyFile puts the file at key, unless the bucket has an object there of the file's size.
func copyFile(ctx context.Context, out store.Store, path, key string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	size, err := out.Stat(ctx, key)
	if err == nil && size == info.Size() {
		log.Printf("kept %s: the bucket has it", key)
		return nil
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := out.Put(ctx, key, file); err != nil {
		return err
	}
	log.Printf("copied %s", key)
	return nil
}
