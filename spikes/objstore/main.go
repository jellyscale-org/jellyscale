// Measures the object store operations jellyscale depends on, against a real
// S3-compatible endpoint: segment write-through, ranged reads, listing, and how
// presigned URLs behave with the headers real clients send.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func pct(d []time.Duration, p float64) time.Duration {
	slices.Sort(d)
	return d[int(float64(len(d)-1)*p)].Round(100 * time.Microsecond)
}

func fetch(method, url string, headers map[string]string) (int, int64, time.Duration) {
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		log.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, 0, 0
	}
	defer resp.Body.Close()
	ttfb := time.Since(start)
	n, _ := io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, n, ttfb
}

func main() {
	endpoint := flag.String("endpoint", "", "S3 endpoint")
	key := flag.String("key", "", "access key")
	secret := flag.String("secret", "", "secret key")
	bucket := flag.String("bucket", "spike", "bucket, which must exist")
	region := flag.String("region", "us-east-1", "region")
	objects := flag.Int("objects", 2000, "objects for the list test")
	flag.Parse()

	ctx := context.Background()
	client := s3.New(s3.Options{
		BaseEndpoint: endpoint,
		Region:       *region,
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider(*key, *secret, ""),
	})

	// Segment write-through: about 4s of 8Mbps video, 200 concurrent streams
	// would produce 50 of these per second.
	segment := make([]byte, 2<<20)
	if _, err := rand.Read(segment); err != nil {
		log.Fatal(err)
	}
	for _, workers := range []int{1, 16, 64} {
		var mu sync.Mutex
		var latencies []time.Duration
		var wg sync.WaitGroup
		start := time.Now()
		for w := range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range 20 {
					begin := time.Now()
					_, err := client.PutObject(ctx, &s3.PutObjectInput{
						Bucket: bucket, Key: aws.String(fmt.Sprintf("seg/%d/%d", w, i)), Body: bytes.NewReader(segment),
					})
					if err != nil {
						log.Fatal(err)
					}
					mu.Lock()
					latencies = append(latencies, time.Since(begin))
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		rate := float64(len(latencies)) / time.Since(start).Seconds()
		fmt.Printf("put 2MB x%-3d    p50 %-9s p95 %-9s %7.1f/s\n", workers, pct(latencies, .5), pct(latencies, .95), rate)
	}

	// Listing, as the scanner would.
	small := []byte("x")
	var wg sync.WaitGroup
	sem := make(chan struct{}, 64)
	for i := range *objects {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			k := fmt.Sprintf("lib/show%03d/s01e%04d.mkv", i%100, i)
			if _, err := client.PutObject(ctx, &s3.PutObjectInput{Bucket: bucket, Key: &k, Body: bytes.NewReader(small)}); err != nil {
				log.Fatal(err)
			}
		}()
	}
	wg.Wait()
	begin := time.Now()
	listed := 0
	pages := s3.NewListObjectsV2Paginator(client, &s3.ListObjectsV2Input{Bucket: bucket, Prefix: aws.String("lib/")})
	etags := 0
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			log.Fatal(err)
		}
		listed += len(page.Contents)
		for _, o := range page.Contents {
			if o.ETag != nil && o.Size != nil {
				etags++
			}
		}
	}
	fmt.Printf("list            %d objects in %s, %d with size and etag\n", listed, time.Since(begin).Round(time.Millisecond), etags)

	// Read path and presigned URL behavior.
	big := bytes.Repeat(segment, 64)
	if _, err := client.PutObject(ctx, &s3.PutObjectInput{Bucket: bucket, Key: aws.String("media.mkv"), Body: bytes.NewReader(big)}); err != nil {
		log.Fatal(err)
	}
	presigner := s3.NewPresignClient(client)
	get, err := presigner.PresignGetObject(ctx, &s3.GetObjectInput{Bucket: bucket, Key: aws.String("media.mkv")})
	if err != nil {
		log.Fatal(err)
	}
	var ttfbs []time.Duration
	for i := range 50 {
		rng := fmt.Sprintf("bytes=%d-%d", i*1<<20, i*1<<20+(1<<20)-1)
		status, _, ttfb := fetch("GET", get.URL, map[string]string{"Range": rng})
		if status != http.StatusPartialContent {
			log.Fatalf("range read returned %d", status)
		}
		ttfbs = append(ttfbs, ttfb)
	}
	fmt.Printf("range 1MB       ttfb p50 %-9s p95 %s\n", pct(ttfbs, .5), pct(ttfbs, .95))

	begin = time.Now()
	status, n, _ := fetch("GET", get.URL, nil)
	fmt.Printf("full read       %d, %.0f MB/s\n", status, float64(n)/1e6/time.Since(begin).Seconds())

	for _, check := range []struct {
		name, method string
		headers      map[string]string
	}{
		{"presigned GET", "GET", map[string]string{"Range": "bytes=0-99"}},
		{"presigned HEAD", "HEAD", nil},
		{"with MediaBrowser auth", "GET", map[string]string{"Range": "bytes=0-99", "Authorization": `MediaBrowser Token="abc"`}},
		{"with Bearer auth", "GET", map[string]string{"Range": "bytes=0-99", "Authorization": "Bearer abc"}},
		{"with Origin", "GET", map[string]string{"Range": "bytes=0-99", "Origin": "https://example.test"}},
	} {
		status, _, _ := fetch(check.method, get.URL, check.headers)
		fmt.Printf("%-23s -> %d\n", check.name, status)
	}
	fmt.Printf("URL %s\n", get.URL)
}
