package main

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestValidateArguments_RejectsNegative — every numeric flag must reject
// negative values so they don't silently cast to enormous uint values.
func TestValidateArguments_RejectsNegative(t *testing.T) {
	tests := []struct {
		name string
		mut  func(*arguments)
		flag string
	}{
		{"depth", func(a *arguments) { a.Depth = -1 }, "--depth"},
		{"timeout", func(a *arguments) { a.Timeout = -1 }, "--timeout"},
		{"delay", func(a *arguments) { a.Delay = -1 }, "--delay"},
		{"concurrency", func(a *arguments) { a.Concurrency = -1 }, "--concurrency"},
		{"rate_limit", func(a *arguments) { a.RateLimit = -1 }, "--rate-limit"},
		{"imagequality", func(a *arguments) { a.ImageQuality = -1 }, "--imagequality"},
		{"serverport", func(a *arguments) { a.ServerPort = -1 }, "--serverport"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var args arguments
			tt.mut(&args)
			err := validateArguments(&args)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.flag, "error must mention the flag name")
		})
	}
}

// TestValidateArguments_RejectsAboveCap — sanity caps so a typo can't
// produce a multi-day sleep or runaway worker pool.
func TestValidateArguments_RejectsAboveCap(t *testing.T) {
	tests := []struct {
		name string
		mut  func(*arguments)
		flag string
	}{
		{"depth", func(a *arguments) { a.Depth = maxDepth + 1 }, "--depth"},
		{"timeout", func(a *arguments) { a.Timeout = maxTimeoutSecs + 1 }, "--timeout"},
		{"delay", func(a *arguments) { a.Delay = maxDelayMillis + 1 }, "--delay"},
		{"concurrency", func(a *arguments) { a.Concurrency = maxConcurrency + 1 }, "--concurrency"},
		{"rate_limit", func(a *arguments) { a.RateLimit = maxRateLimit + 1 }, "--rate-limit"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var args arguments
			tt.mut(&args)
			err := validateArguments(&args)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.flag, "error must mention the flag name")
		})
	}
}

// TestValidateArguments_RejectsNonFiniteRate — NaN/Inf compare false against
// every bound so they used to slip past min/max checks; they must be rejected.
func TestValidateArguments_RejectsNonFiniteRate(t *testing.T) {
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		args := arguments{RateLimit: v}
		err := validateArguments(&args)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--rate-limit", "error must mention flag name")
	}
}

// TestValidateArguments_AcceptsZeroAndDefaults — zero is the documented
// "use default / unlimited / none" sentinel and must pass.
func TestValidateArguments_AcceptsZeroAndDefaults(t *testing.T) {
	var args arguments
	require.NoError(t, validateArguments(&args), "zero-valued args must pass validation")
}

// TestValidateArguments_AcceptsBoundary — exact cap values are valid.
func TestValidateArguments_AcceptsBoundary(t *testing.T) {
	args := arguments{
		Depth:       maxDepth,
		Timeout:     maxTimeoutSecs,
		Delay:       maxDelayMillis,
		Concurrency: maxConcurrency,
		RateLimit:   maxRateLimit,
	}
	require.NoError(t, validateArguments(&args))
}
