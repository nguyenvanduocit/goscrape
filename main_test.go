package main

import (
	"math"
	"os"
	"path/filepath"
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

// TestReadURLFile_SkipsBlankAndComments — one URL per line, with whitespace
// trimmed; blank lines and #-comment lines (even indented) are ignored.
func TestReadURLFile_SkipsBlankAndComments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "urls.txt")
	content := "https://a.example/1\n" +
		"  \n" +
		"# a comment\n" +
		"  https://a.example/2  \n" +
		"\t# indented comment\n" +
		"https://a.example/3\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0600))

	urls, err := readURLFile(path)
	require.NoError(t, err)
	assert.Equal(t, []string{
		"https://a.example/1",
		"https://a.example/2",
		"https://a.example/3",
	}, urls)
}

// TestReadURLFile_MissingFile — a missing file is a hard error with an
// actionable message, not a silent empty list.
func TestReadURLFile_MissingFile(t *testing.T) {
	_, err := readURLFile(filepath.Join(t.TempDir(), "does-not-exist.txt"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reading url file")
}

// TestMergeURLSources_DedupesPositionalAndFile — file URLs append to
// positional URLs and duplicates collapse, preserving first-seen order.
func TestMergeURLSources_DedupesPositionalAndFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "urls.txt")
	require.NoError(t, os.WriteFile(path,
		[]byte("https://a.example/2\nhttps://a.example/3\n"), 0600))

	args := arguments{
		URLs:    []string{"https://a.example/1", "https://a.example/2"},
		URLFile: path,
	}
	require.NoError(t, mergeURLSources(&args))
	assert.Equal(t, []string{
		"https://a.example/1",
		"https://a.example/2",
		"https://a.example/3",
	}, args.URLs)
}

// TestMergeURLSources_NoFile_Unchanged — without --url-file the positional
// URLs are left exactly as-is.
func TestMergeURLSources_NoFile_Unchanged(t *testing.T) {
	args := arguments{URLs: []string{"https://a.example/1"}}
	require.NoError(t, mergeURLSources(&args))
	assert.Equal(t, []string{"https://a.example/1"}, args.URLs)
}
