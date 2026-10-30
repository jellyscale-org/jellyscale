// Command putfile writes a local file into a store, so e2e fixtures reach S3
// through the same code the server uses.
// Usage: putfile <store-uri> <key> <file>
package main

import (
	"context"
	"fmt"
	"os"

	"jellyscale/internal/store"
)

func main() {
	if len(os.Args) != 4 {
		fmt.Fprintln(os.Stderr, "usage: putfile <store-uri> <key> <file>")
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Args[2], os.Args[3]); err != nil {
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
