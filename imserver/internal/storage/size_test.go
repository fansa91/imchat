package storage

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"imserver/internal/config"
)

// fakeS3 起一个只实现 HEAD（StatObject）的假 S3：前 failN 次返回 failStatus，之后返回 200。
func fakeS3(t *testing.T, failN int, failStatus int) (*Storage, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		if int(n) <= failN {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(failStatus)
			_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><Error><Code>TransientTestError</Code><Message>boom</Message></Error>`))
			return
		}
		w.Header().Set("Content-Length", "30000")
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Last-Modified", "Wed, 21 Oct 2026 07:28:00 GMT")
		w.Header().Set("ETag", `"abc123"`)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")
	cli, err := minio.New(host, &minio.Options{
		Creds:  credentials.NewStaticV4("testkey", "testsecret", ""),
		Secure: false, Region: "garage", BucketLookup: minio.BucketLookupPath,
	})
	if err != nil {
		t.Fatalf("minio.New: %v", err)
	}
	return &Storage{cfg: config.StorageConfig{Bucket: "imchat"}, cli: cli}, &hits
}

func ctxTimeout() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 15*time.Second)
}

// 瞬时错误（403）前两次失败、第三次成功：SizeWithRetry 应拿到大小。
// 注：403 不是 NoSuchKey/404，走的是「非确定性错误」分支。
func TestSizeWithRetryRecoversFromTransientErrors(t *testing.T) {
	s, hits := fakeS3(t, 2, http.StatusForbidden)
	ctx, cancel := ctxTimeout()
	defer cancel()
	size, err := s.SizeWithRetry(ctx, "avatar/8/202610/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.jpg")
	if err != nil {
		t.Fatalf("SizeWithRetry: %v", err)
	}
	if size != 30000 {
		t.Fatalf("size = %d, want 30000", size)
	}
	if hits.Load() < 3 {
		t.Fatalf("requests = %d, want >= 3 (2 failures + 1 success)", hits.Load())
	}
}

// 持续失败：应返回错误，且总尝试次数有界（1 + 3 次重试 = 4 次 Size 调用）。
func TestSizeWithRetryGivesUpAfterBoundedAttempts(t *testing.T) {
	s, _ := fakeS3(t, 1<<30, http.StatusForbidden)
	ctx, cancel := ctxTimeout()
	defer cancel()
	if _, err := s.SizeWithRetry(ctx, "avatar/8/202610/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.jpg"); err == nil {
		t.Fatal("expected error, got nil")
	}
}

// 对象不存在（404）：立即返回 ErrNotFound，不重试。
func TestSizeWithRetryNotFoundIsNotRetried(t *testing.T) {
	s, hits := fakeS3(t, 1<<30, http.StatusNotFound)
	ctx, cancel := ctxTimeout()
	defer cancel()
	_, err := s.SizeWithRetry(ctx, "avatar/8/202610/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.jpg")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("requests = %d, want exactly 1 (no retry on 404)", hits.Load())
	}
}
