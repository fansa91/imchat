package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"imserver/internal/config"
)

// TestSFUNewSessionSendsNoBody 回归测试（2026-10-22 生产实测）：
// Cloudflare sessions/new 不收请求体，带 {} 会回 400 decoding_error
// （Body JSON validation error: sessionDescription），导致多人通话 502。
func TestSFUNewSessionSendsNoBody(t *testing.T) {
	var gotBody []byte
	var gotPath, gotAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if len(strings.TrimSpace(string(gotBody))) > 0 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"errorCode":"decoding_error","errorDescription":"Body JSON validation error: sessionDescription"}`))
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"sessionId":"sess-test"}`))
	}))
	defer ts.Close()

	s := &Server{cfg: &config.Config{Calls: config.CallsConfig{
		BaseURL: ts.URL, SFUAppID: "appid32", SFUAppSecret: "secret64",
	}}}
	sid, err := s.sfuNewSession(context.Background())
	if err != nil {
		t.Fatalf("sfuNewSession: %v", err)
	}
	if sid != "sess-test" {
		t.Fatalf("sessionId = %q, want sess-test", sid)
	}
	if len(gotBody) != 0 {
		t.Fatalf("request body = %q, want empty", gotBody)
	}
	if gotPath != "/apps/appid32/sessions/new" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotAuth != "Bearer secret64" {
		t.Fatalf("authorization = %q", gotAuth)
	}
}
