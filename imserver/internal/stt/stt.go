// Package stt 语音转文字（v0.3.0）：Groq（OpenAI 兼容 /audio/transcriptions）与 Cloudflare Workers AI。
// 主提供方失败（5xx / 超时 / 429 / 网络错误）时尝试 fallback。
package stt

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"imserver/internal/config"
)

// ProviderError 提供方返回的错误。Retryable 表示可以尝试 fallback。
type ProviderError struct {
	Provider  string
	Status    int
	Retryable bool
	Msg       string
}

func (e *ProviderError) Error() string {
	return fmt.Sprintf("stt %s: status %d: %s", e.Provider, e.Status, e.Msg)
}

type Transcriber struct {
	cfg  config.STTConfig
	http *http.Client
}

// New cfg.Enabled=false 时返回 nil。
func New(cfg config.STTConfig) *Transcriber {
	if !cfg.Enabled {
		return nil
	}
	if cfg.Fallback != "" && !cfg.Configured(cfg.Fallback) {
		slog.Warn("stt.fallback 未配置凭据，已忽略", "fallback", cfg.Fallback)
		cfg.Fallback = ""
	}
	return &Transcriber{cfg: cfg, http: &http.Client{}}
}

func (t *Transcriber) Config() config.STTConfig { return t.cfg }

// Transcribe 转写音频，返回文本和实际使用的提供方。
func (t *Transcriber) Transcribe(ctx context.Context, audio []byte, mime string) (text, provider string, err error) {
	providers := []string{t.cfg.Provider}
	if t.cfg.Fallback != "" {
		providers = append(providers, t.cfg.Fallback)
	}
	for i, p := range providers {
		text, err = t.call(ctx, p, audio, mime)
		if err == nil {
			return text, p, nil
		}
		slog.Warn("stt provider failed", "provider", p, "err", err)
		var pe *ProviderError
		if errors.As(err, &pe) && !pe.Retryable {
			return "", p, err
		}
		if ctx.Err() != nil || i == len(providers)-1 {
			return "", p, err
		}
	}
	return "", "", err
}

func (t *Transcriber) call(ctx context.Context, provider string, audio []byte, mime string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, time.Duration(t.cfg.TimeoutSeconds)*time.Second)
	defer cancel()
	var req *http.Request
	var err error
	switch provider {
	case "groq":
		req, err = t.groqRequest(cctx, audio, mime)
	case "cloudflare":
		req, err = t.cloudflareRequest(cctx, audio)
	default:
		return "", fmt.Errorf("未知 stt 提供方 %q", provider)
	}
	if err != nil {
		return "", err
	}
	resp, err := t.http.Do(req)
	if err != nil {
		// 超时 / 网络错误：可重试
		return "", &ProviderError{Provider: provider, Retryable: true, Msg: err.Error()}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", &ProviderError{Provider: provider, Status: resp.StatusCode, Retryable: true, Msg: err.Error()}
	}
	if resp.StatusCode != http.StatusOK {
		return "", &ProviderError{
			Provider: provider, Status: resp.StatusCode,
			Retryable: resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests,
			Msg:       truncate(string(body), 300),
		}
	}
	switch provider {
	case "groq":
		var out struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			return "", &ProviderError{Provider: provider, Status: resp.StatusCode, Msg: "响应解析失败: " + truncate(string(body), 200)}
		}
		return strings.TrimSpace(out.Text), nil
	default:
		var out struct {
			Success bool `json:"success"`
			Result  struct {
				Text string `json:"text"`
			} `json:"result"`
		}
		if err := json.Unmarshal(body, &out); err != nil || !out.Success {
			return "", &ProviderError{Provider: provider, Status: resp.StatusCode, Msg: "响应解析失败: " + truncate(string(body), 200)}
		}
		return strings.TrimSpace(out.Result.Text), nil
	}
}

// groqRequest POST {base}/audio/transcriptions，multipart: file, model, language, response_format=json。
func (t *Transcriber) groqRequest(ctx context.Context, audio []byte, mime string) (*http.Request, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "audio."+extFor(mime))
	if err != nil {
		return nil, err
	}
	_, _ = fw.Write(audio)
	_ = mw.WriteField("model", t.cfg.Groq.Model)
	if t.cfg.Language != "" {
		_ = mw.WriteField("language", t.cfg.Language)
	}
	_ = mw.WriteField("response_format", "json")
	if err := mw.Close(); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.cfg.Groq.BaseURL+"/audio/transcriptions", &buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+t.cfg.Groq.APIKey)
	return req, nil
}

// cloudflareRequest POST {base}/accounts/{account_id}/ai/run/{model}，JSON {"audio": base64, "language"}。
func (t *Transcriber) cloudflareRequest(ctx context.Context, audio []byte) (*http.Request, error) {
	payload := map[string]any{"audio": base64.StdEncoding.EncodeToString(audio)}
	if t.cfg.Language != "" {
		payload["language"] = t.cfg.Language
	}
	b, _ := json.Marshal(payload)
	u := fmt.Sprintf("%s/accounts/%s/ai/run/%s", t.cfg.Cloudflare.BaseURL, t.cfg.Cloudflare.AccountID, t.cfg.Cloudflare.Model)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+t.cfg.Cloudflare.APIToken)
	return req, nil
}

func extFor(mime string) string {
	switch strings.ToLower(strings.SplitN(mime, ";", 2)[0]) {
	case "audio/webm":
		return "webm"
	case "audio/ogg":
		return "ogg"
	case "audio/mp4", "audio/aac", "audio/x-m4a", "audio/m4a":
		return "m4a"
	case "audio/mpeg":
		return "mp3"
	}
	return "webm"
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
