package testenv

import "testing"

func TestProofOfSkip(t *testing.T)    { t.Skip("throwaway: a skip must fail the run") }
func TestProofOfFailure(t *testing.T) { t.Fatalf("throwaway: this failure must be annotated here") }
