package store

import (
	"os"
	"testing"
)

// TestS3RoundTrip exercises the S3 driver against a real S3-compatible server (MinIO).
// Skipped unless S3_TEST_ENDPOINT is set (e.g. localhost:9000).
func TestS3RoundTrip(t *testing.T) {
	ep := os.Getenv("S3_TEST_ENDPOINT")
	if ep == "" {
		t.Skip("set S3_TEST_ENDPOINT (+ S3_TEST_KEY/S3_TEST_SECRET) to run")
	}
	s, err := NewS3Storage(ep, "msb-test", "us-east-1",
		os.Getenv("S3_TEST_KEY"), os.Getenv("S3_TEST_SECRET"), false)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	key := "books/bk-test.epub"
	if err := s.Put(key, []byte("hello s3")); err != nil {
		t.Fatalf("put: %v", err)
	}
	if !s.Has(key) {
		t.Error("Has=false after put")
	}
	got, err := s.Get(key)
	if err != nil || string(got) != "hello s3" {
		t.Errorf("get: %v %q", err, got)
	}
	if err := s.Delete(key); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if s.Has(key) {
		t.Error("Has=true after delete")
	}
}
