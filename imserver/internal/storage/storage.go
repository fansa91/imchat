// Package storage 封装 S3 兼容存储（推荐 Garage，也兼容 MinIO 等）：预签名上传/下载、校验、生命周期、CORS。
// 只用标准 S3 API（SigV4、path-style），不依赖任何厂商扩展。
//
// 上传流程（客户端直传，不经过 imserver）：
//  1. 客户端 POST /api/uploads/presign 申请上传，服务端校验类型/大小后生成对象 key 和预签名 PUT URL
//  2. 客户端直接 PUT 文件到对象存储
//  3. 客户端发送消息时带上 key，服务端 StatObject + 读取文件头校验真实大小和类型，通过后才落库
//
// key 格式：{kind}/{uid}/{yyyymm}/{32位随机hex}.{ext}，kind 为 voice 或 image（v0.3.0）。
// key 中带上传者 uid，发送消息时校验只能引用自己上传的文件。
package storage

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/cors"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/minio/minio-go/v7/pkg/lifecycle"

	"imserver/internal/config"
)

var (
	ErrDisabled = errors.New("对象存储未启用")
	ErrNotFound = errors.New("对象不存在或已过期")
)

// LifecycleRuleID 自动设置的生命周期规则 ID。
const (
	LifecycleRuleID      = "imserver-voice-expire"
	ImageLifecycleRuleID = "imserver-image-expire" // 仅 image_ttl_days > 0 时设置
)

type Storage struct {
	cfg     config.StorageConfig
	cli     *minio.Client // 服务端内网访问
	presign *minio.Client // 只用于生成预签名 URL（host 为浏览器可访问的公网地址）
}

// New 连接对象存储，确保 bucket 存在。cfg.Enabled=false 时返回 nil, nil。
func New(ctx context.Context, cfg config.StorageConfig) (*Storage, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	creds := credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, "")
	cli, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds: creds, Secure: cfg.UseSSL, Region: cfg.Region, BucketLookup: minio.BucketLookupPath,
	})
	if err != nil {
		return nil, err
	}
	host, secure, err := cfg.PublicURL()
	if err != nil {
		return nil, err
	}
	// Region 必须显式设置，否则预签名前会发请求查询 bucket 所在区域
	pcli, err := minio.New(host, &minio.Options{
		Creds: creds, Secure: secure, Region: cfg.Region, BucketLookup: minio.BucketLookupPath,
	})
	if err != nil {
		return nil, err
	}
	s := &Storage{cfg: cfg, cli: cli, presign: pcli}

	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ok, err := cli.BucketExists(cctx, cfg.Bucket)
	if err != nil {
		return nil, fmt.Errorf("连接对象存储失败: %w", err)
	}
	if !ok {
		if err := cli.MakeBucket(cctx, cfg.Bucket, minio.MakeBucketOptions{Region: cfg.Region}); err != nil {
			return nil, fmt.Errorf("bucket %q 不存在且无法创建（Garage 请先执行 garage bucket create %s 并 garage bucket allow --read --write --owner）: %w",
				cfg.Bucket, cfg.Bucket, err)
		}
		slog.Info("storage bucket created", "bucket", cfg.Bucket)
	}
	if cfg.SetLifecycle {
		if err := s.EnsureLifecycle(cctx); err != nil {
			// 不阻止启动：部分 S3 兼容存储不支持生命周期 API，或 key 没有 owner 权限
			slog.Warn("⚠️ 无法自动设置 bucket 生命周期规则（语音不会自动过期！）。请确认 access key 有 owner 权限（garage bucket allow --owner），或手动设置 voice/ 前缀 "+
				fmt.Sprint(cfg.VoiceTTLDays)+" 天过期", "bucket", cfg.Bucket, "err", err)
		} else {
			slog.Info("storage lifecycle ensured", "bucket", cfg.Bucket, "voice_expire_days", cfg.VoiceTTLDays, "image_expire_days", cfg.ImageTTLDays)
		}
		if err := s.EnsureCORS(cctx); err != nil {
			slog.Warn("⚠️ 无法自动设置 bucket CORS（浏览器直传/下载会被拦截）。请确认 access key 有 owner 权限，或在存储/反向代理上手动配置 CORS",
				"bucket", cfg.Bucket, "origins", s.corsOrigins(), "err", err)
		} else {
			slog.Info("storage CORS ensured", "bucket", cfg.Bucket, "origins", s.corsOrigins())
		}
	}
	return s, nil
}

func (s *Storage) Config() config.StorageConfig { return s.cfg }

// EnsureLifecycle 给 voice/ 前缀设置 N 天过期规则；image_ttl_days > 0 时给 image/ 前缀设置规则，
// 为 0（永久）时删除之前可能设置过的图片规则。保留 bucket 上已有的其他规则。
func (s *Storage) EnsureLifecycle(ctx context.Context) error {
	conf, err := s.cli.GetBucketLifecycle(ctx, s.cfg.Bucket)
	if err != nil {
		if minio.ToErrorResponse(err).Code != "NoSuchLifecycleConfiguration" {
			return err
		}
		conf = lifecycle.NewConfiguration()
	}
	if conf == nil {
		conf = lifecycle.NewConfiguration()
	}
	want := []lifecycle.Rule{{
		ID:         LifecycleRuleID,
		Status:     "Enabled",
		RuleFilter: lifecycle.Filter{Prefix: "voice/"},
		Expiration: lifecycle.Expiration{Days: lifecycle.ExpirationDays(s.cfg.VoiceTTLDays)},
	}}
	if s.cfg.ImageTTLDays > 0 {
		want = append(want, lifecycle.Rule{
			ID:         ImageLifecycleRuleID,
			Status:     "Enabled",
			RuleFilter: lifecycle.Filter{Prefix: "image/"},
			Expiration: lifecycle.Expiration{Days: lifecycle.ExpirationDays(s.cfg.ImageTTLDays)},
		})
	}
	same := func(a, b lifecycle.Rule) bool {
		return a.Status == b.Status && a.Expiration.Days == b.Expiration.Days && a.RuleFilter.Prefix == b.RuleFilter.Prefix
	}
	// 已是期望状态则不写
	existing := map[string]lifecycle.Rule{}
	for _, r := range conf.Rules {
		if r.ID == LifecycleRuleID || r.ID == ImageLifecycleRuleID {
			existing[r.ID] = r
		}
	}
	upToDate := len(existing) == len(want)
	for _, w := range want {
		if r, ok := existing[w.ID]; !ok || !same(r, w) {
			upToDate = false
		}
	}
	if upToDate {
		return nil
	}
	rules := want
	for _, r := range conf.Rules {
		if r.ID == LifecycleRuleID || r.ID == ImageLifecycleRuleID {
			continue
		}
		rules = append(rules, r)
	}
	conf.Rules = rules
	return s.cli.SetBucketLifecycle(ctx, s.cfg.Bucket, conf)
}

func (s *Storage) corsOrigins() []string {
	if len(s.cfg.CORSOrigins) == 0 {
		return []string{"*"}
	}
	return s.cfg.CORSOrigins
}

// EnsureCORS 允许网页直接 PUT 上传、GET 下载（预签名 URL 跨域访问存储域名）。
func (s *Storage) EnsureCORS(ctx context.Context) error {
	return s.cli.SetBucketCors(ctx, s.cfg.Bucket, cors.NewConfig([]cors.Rule{{
		ID:            "imserver",
		AllowedOrigin: s.corsOrigins(),
		AllowedMethod: []string{"GET", "PUT", "HEAD"},
		AllowedHeader: []string{"*"},
		ExposeHeader:  []string{"ETag", "Content-Length", "Content-Type"},
		MaxAgeSeconds: 3600,
	}}))
}

// LifecycleDays 返回当前 bucket 上本服务规则的过期天数（0 表示没有）。
func (s *Storage) LifecycleDays(ctx context.Context) (int, error) {
	conf, err := s.cli.GetBucketLifecycle(ctx, s.cfg.Bucket)
	if err != nil {
		return 0, err
	}
	for _, r := range conf.Rules {
		if r.ID == LifecycleRuleID && r.Status == "Enabled" {
			return int(r.Expiration.Days), nil
		}
	}
	return 0, nil
}

// ---------- key ----------

var keyRe = regexp.MustCompile(`^(voice|image)/(\d{1,20})/(\d{6})/([a-f0-9]{32})\.([a-z0-9]{2,5})$`)

// ParseKey 校验 key 格式，返回 kind 与上传者 uid。
func ParseKey(key string) (kind string, uid uint64, ok bool) {
	m := keyRe.FindStringSubmatch(key)
	if m == nil {
		return "", 0, false
	}
	uid, err := strconv.ParseUint(m[2], 10, 64)
	return m[1], uid, err == nil
}

func NewKey(kind string, uid uint64, ext string) string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%s/%d/%s/%s.%s", kind, uid, time.Now().UTC().Format("200601"), hex.EncodeToString(b), ext)
}

// ---------- 语音类型 ----------

// voiceMimes 允许的语音 MIME → 扩展名。
// Chrome/Firefox/Edge 录制 audio/webm(opus)，Safari 录制 audio/mp4(aac)。
var voiceMimes = map[string]string{
	"audio/webm": "webm",
	"audio/ogg":  "ogg",
	"audio/mp4":  "m4a",
	"audio/aac":  "aac",
	"audio/mpeg": "mp3",
}

// NormalizeVoiceMime 去掉 ;codecs= 参数并校验，返回基础 MIME 和扩展名。
func NormalizeVoiceMime(m string) (mime, ext string, ok bool) {
	m = strings.ToLower(strings.TrimSpace(strings.SplitN(m, ";", 2)[0]))
	if m == "audio/x-m4a" || m == "audio/m4a" {
		m = "audio/mp4"
	}
	ext, ok = voiceMimes[m]
	return m, ext, ok
}

// sniffAudio 根据文件头判断真实格式，返回基础 MIME。
func sniffAudio(h []byte) string {
	has := func(off int, sig string) bool { return len(h) >= off+len(sig) && string(h[off:off+len(sig)]) == sig }
	switch {
	case has(0, "\x1a\x45\xdf\xa3"):
		return "audio/webm" // EBML（webm / mkv）
	case has(0, "OggS"):
		return "audio/ogg"
	case has(4, "ftyp"):
		return "audio/mp4"
	case has(0, "ID3"), len(h) >= 2 && h[0] == 0xFF && (h[1]&0xE0) == 0xE0 && (h[1]&0x06) != 0:
		return "audio/mpeg"
	case len(h) >= 2 && h[0] == 0xFF && (h[1]&0xF6) == 0xF0:
		return "audio/aac"
	}
	return ""
}

// ---------- 图片类型（v0.3.0） ----------

// imageMimes 允许的图片 MIME → 扩展名。
var imageMimes = map[string]string{
	"image/jpeg": "jpg",
	"image/png":  "png",
	"image/webp": "webp",
	"image/gif":  "gif",
}

// NormalizeImageMime 校验图片 MIME（image/jpg 视为 image/jpeg），返回基础 MIME 和扩展名。
func NormalizeImageMime(m string) (mime, ext string, ok bool) {
	m = strings.ToLower(strings.TrimSpace(strings.SplitN(m, ";", 2)[0]))
	if m == "image/jpg" || m == "image/pjpeg" {
		m = "image/jpeg"
	}
	ext, ok = imageMimes[m]
	return m, ext, ok
}

// sniffImage 根据文件头判断真实图片格式。
func sniffImage(h []byte) string {
	has := func(off int, sig string) bool { return len(h) >= off+len(sig) && string(h[off:off+len(sig)]) == sig }
	switch {
	case has(0, "\xff\xd8\xff"):
		return "image/jpeg"
	case has(0, "\x89PNG\r\n\x1a\n"):
		return "image/png"
	case has(0, "RIFF") && has(8, "WEBP"):
		return "image/webp"
	case has(0, "GIF87a"), has(0, "GIF89a"):
		return "image/gif"
	}
	return ""
}

// ---------- 预签名 ----------

func (s *Storage) PresignPut(ctx context.Context, key string) (string, time.Time, error) {
	d := time.Duration(s.cfg.PresignPutSeconds) * time.Second
	u, err := s.presign.PresignedPutObject(ctx, s.cfg.Bucket, key, d)
	if err != nil {
		return "", time.Time{}, err
	}
	return u.String(), time.Now().Add(d), nil
}

func (s *Storage) PresignGet(ctx context.Context, key, mime string) (string, time.Time, error) {
	d := time.Duration(s.cfg.PresignGetSeconds) * time.Second
	params := url.Values{}
	if mime != "" {
		params.Set("response-content-type", mime)
	}
	u, err := s.presign.PresignedGetObject(ctx, s.cfg.Bucket, key, d, params)
	if err != nil {
		return "", time.Time{}, err
	}
	return u.String(), time.Now().Add(d), nil
}

// ObjectInfo 校验后的对象信息。
type ObjectInfo struct {
	Size int64
	Mime string
}

// Exists 对象是否还在（语音过期后会被生命周期删除）。
func (s *Storage) Exists(ctx context.Context, key string) (bool, error) {
	_, err := s.cli.StatObject(ctx, s.cfg.Bucket, key, minio.StatObjectOptions{})
	if err != nil {
		if r := minio.ToErrorResponse(err); r.Code == "NoSuchKey" || r.StatusCode == http.StatusNotFound {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// VerifyVoice 校验已上传的语音对象：存在、大小不超限、文件头是允许的音频格式且与声明一致。
// 不合格的对象会被删除。
func (s *Storage) VerifyVoice(ctx context.Context, key, declaredMime string) (*ObjectInfo, error) {
	st, err := s.cli.StatObject(ctx, s.cfg.Bucket, key, minio.StatObjectOptions{})
	if err != nil {
		if r := minio.ToErrorResponse(err); r.Code == "NoSuchKey" || r.StatusCode == http.StatusNotFound {
			return nil, ErrNotFound
		}
		return nil, err
	}
	reject := func(msg string) (*ObjectInfo, error) {
		_ = s.cli.RemoveObject(context.Background(), s.cfg.Bucket, key, minio.RemoveObjectOptions{})
		return nil, errors.New(msg)
	}
	if st.Size <= 0 || st.Size > s.cfg.VoiceMaxBytes {
		return reject(fmt.Sprintf("语音文件大小超出限制（最大 %d KB）", s.cfg.VoiceMaxBytes/1024))
	}
	opts := minio.GetObjectOptions{}
	_ = opts.SetRange(0, 15)
	obj, err := s.cli.GetObject(ctx, s.cfg.Bucket, key, opts)
	if err != nil {
		return nil, err
	}
	defer obj.Close()
	head, err := io.ReadAll(io.LimitReader(obj, 16))
	if err != nil {
		return nil, err
	}
	real := sniffAudio(head)
	if real == "" {
		return reject("不是有效的音频文件")
	}
	decl, _, _ := NormalizeVoiceMime(declaredMime)
	// aac 裸流和 mp4 容器在 Safari 上都可能出现，放宽为同一族
	if decl != real && !(isAAC(decl) && isAAC(real)) {
		return reject("音频格式与声明不符")
	}
	return &ObjectInfo{Size: st.Size, Mime: real}, nil
}

// VerifyImage 校验已上传的图片对象：存在、大小不超限、文件头是允许的图片格式且与声明一致。
// 不合格的对象会被删除。
func (s *Storage) VerifyImage(ctx context.Context, key, declaredMime string) (*ObjectInfo, error) {
	st, err := s.cli.StatObject(ctx, s.cfg.Bucket, key, minio.StatObjectOptions{})
	if err != nil {
		if r := minio.ToErrorResponse(err); r.Code == "NoSuchKey" || r.StatusCode == http.StatusNotFound {
			return nil, ErrNotFound
		}
		return nil, err
	}
	reject := func(msg string) (*ObjectInfo, error) {
		_ = s.cli.RemoveObject(context.Background(), s.cfg.Bucket, key, minio.RemoveObjectOptions{})
		return nil, errors.New(msg)
	}
	if st.Size <= 0 || st.Size > s.cfg.ImageMaxBytes {
		return reject(fmt.Sprintf("图片大小超出限制（最大 %d MB）", s.cfg.ImageMaxBytes>>20))
	}
	opts := minio.GetObjectOptions{}
	_ = opts.SetRange(0, 15)
	obj, err := s.cli.GetObject(ctx, s.cfg.Bucket, key, opts)
	if err != nil {
		return nil, err
	}
	defer obj.Close()
	head, err := io.ReadAll(io.LimitReader(obj, 16))
	if err != nil {
		return nil, err
	}
	real := sniffImage(head)
	if real == "" {
		return reject("不是有效的图片文件")
	}
	if decl, _, _ := NormalizeImageMime(declaredMime); decl != real {
		return reject("图片格式与声明不符")
	}
	return &ObjectInfo{Size: st.Size, Mime: real}, nil
}

// Read 读取整个对象（语音转文字用），超过 maxBytes 报错。对象不存在返回 ErrNotFound。
func (s *Storage) Read(ctx context.Context, key string, maxBytes int64) ([]byte, error) {
	obj, err := s.cli.GetObject(ctx, s.cfg.Bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	defer obj.Close()
	b, err := io.ReadAll(io.LimitReader(obj, maxBytes+1))
	if err != nil {
		if r := minio.ToErrorResponse(err); r.Code == "NoSuchKey" || r.StatusCode == http.StatusNotFound {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if int64(len(b)) > maxBytes {
		return nil, fmt.Errorf("对象超过 %d 字节", maxBytes)
	}
	return b, nil
}

func isAAC(m string) bool { return m == "audio/mp4" || m == "audio/aac" }

// Remove 删除对象（撤回语音/图片时调用），忽略不存在。
func (s *Storage) Remove(ctx context.Context, key string) error {
	return s.cli.RemoveObject(ctx, s.cfg.Bucket, key, minio.RemoveObjectOptions{})
}
