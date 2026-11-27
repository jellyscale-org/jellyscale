// Command seed copies the films of the fixtures image into an S3 bucket, once, for the S3 library of
// deploy/fixtures/compose.yaml. The image holds each film once, so each copy is renamed: the title gets " S3", with
// its sidecar files, and the server's item names stay unique. Only the fixtures image holds it.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"jellyscale/internal/store"
)

// film is a directory of the media root and the title its files are named with, without the year.
type film struct{ dir, title string }

// films are the films the S3 library serves beside the library's own: the criteria of test/image.sh.
var films = []film{
	{"movies/Alpha Film (2021)", "Alpha Film"},
	{"transcode/Tau Film (2035)", "Tau Film"},
	{"transcode/Pgs Hevc Film (2090)", "Pgs Hevc Film"},
	{"transcode/Resh Film (2085)", "Resh Film"},
	{"languages/Babel Film", "Babel Film"},
}

func main() {
	endpoint := flag.String("endpoint", "http://seaweedfs:8333", "the S3 endpoint")
	bucket := flag.String("bucket", "jellyscale-fixtures", "the bucket the films go to, under media/")
	root := flag.String("root", "/fixtures/media", "the media root the films are read from")
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
	out, err := store.Open(fmt.Sprintf("s3://%s/media?endpoint=%s", bucket, endpoint))
	if err != nil {
		return err
	}
	for _, movie := range films {
		if err := copyFilm(ctx, out, root, movie); err != nil {
			return fmt.Errorf("copying %s: %w", movie.dir, err)
		}
		log.Printf("copied %s", movie.dir)
	}
	return nil
}

// createBucket retries until the endpoint answers: the seeder starts with SeaweedFS, which may still be starting.
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

// copyFilm puts each file of the film's directory under the copy's directory, with the title renamed.
func copyFilm(ctx context.Context, out store.Store, root string, movie film) error {
	dir := filepath.Join(root, movie.dir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	copyDir := strings.Replace(filepath.Base(dir), movie.title, movie.title+" S3", 1)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if err := putFile(ctx, out, filepath.Join(dir, entry.Name()), copyDir+"/"+strings.Replace(entry.Name(), movie.title, movie.title+" S3", 1)); err != nil {
			return err
		}
	}
	return nil
}

func putFile(ctx context.Context, out store.Store, path, key string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return out.Put(ctx, key, file)
}
