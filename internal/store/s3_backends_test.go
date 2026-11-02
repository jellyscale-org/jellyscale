//go:build !integration

package store

import "testing"

// The tier that needs nothing outside Go has no S3 targets.
func s3Backends(*testing.T) map[string]Store { return nil }
