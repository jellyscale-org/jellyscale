// Command putfile writes a local file into a store, so e2e fixtures reach S3
// through the same code the server uses.
// Usage: putfile <store-uri> <key> <file>
//
//	putfile -delete <store-uri>   removes every object in the store
//
// The second is for test/e2e/run.sh between E2E_REPEAT iterations.
package main

import (
	"context"
	"fmt"
	"os"

	"jellyscale/internal/store"
)

func main() {
	var err error
	switch {
	case len(os.Args) == 3 && os.Args[1] == "-delete":
		err = deleteAll(os.Args[2])
	case len(os.Args) == 4:
		err = run(os.Args[1], os.Args[2], os.Args[3])
	default:
		fmt.Fprintln(os.Stderr, "usage: putfile <store-uri> <key> <file> | putfile -delete <store-uri>")
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "putfile:", err)
		os.Exit(1)
	}
}

func run(uri, key, file string) error {
	target, err := store.Open(uri)
	if err != nil {
		return err
	}
	source, err := os.Open(file)
	if err != nil {
		return err
	}
	defer source.Close()
	return target.Put(context.Background(), key, source)
}

func deleteAll(uri string) error {
	target, err := store.Open(uri)
	if err != nil {
		return err
	}
	ctx := context.Background()
	objects, err := target.List(ctx, "")
	if err != nil {
		return err
	}
	for _, object := range objects {
		if err := target.Delete(ctx, object.Key); err != nil {
			return err
		}
	}
	return nil
}
