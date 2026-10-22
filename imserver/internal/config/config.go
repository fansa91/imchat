package config

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server  ServerConfig  `yaml:"server"`
	MySQL   MySQLConfig   `yaml:"mysql"`
	Redis   RedisConfig   `yaml:"redis"`
	JWT     JWTConfig     `yaml:"jwt"`
	Group   GroupConfig   `yaml:"group"`
	Message MessageConfig `yaml:"message"`
	Storage StorageConfig `yaml:"storage"`
	STT     STTConfig     `yaml:"stt"`
	Calls   CallsConfig   `yaml:"calls"`
	SMTP    SMTPConfig    `yaml:"smtp"`
	Log     LogConfig     `yaml:"log"`
}

type ServerConfig struct {
	Addr         string   `yaml:"addr"`          // 监听地址，如 ":8092"
	NodeID       string   `yaml:"node_id"`       // 多实例部署时区分节点
	AllowOrigins []string `yaml:"allow_origins"` // WebSocket 允许的 Origin，空表示不校验
}

type MySQLConfig struct {
	Host            string `yaml:"host"`
	Port            int    `yaml:"port"`
	User            string `yaml:"user"`
	Password        string `yaml:"password"`
	Database        string `yaml:"database"`
	MaxOpenConns    int    `yaml:"max_open_conns"`
	MaxIdleConns    int    `yaml:"max_idle_conns"`
	ConnMaxLifetime int    `yaml:"conn_max_lifetime_seconds"`
	AutoMigrate     bool   `yaml:"auto_migrate"` // 启动时自动建表/升级
}

type RedisConfig struct {
	Addr     string `yaml:"addr"`
	Password string `yaml:"password"`
	DB       int    `yaml:"db"`
}

type JWTConfig struct {
	Secret   string `yaml:"secret"`
	TTLHours int    `yaml:"ttl_hours"`
}

// GroupConfig 群聊设置。
type GroupConfig struct {
	MaxMembers int `yaml:"max_members"` // 群成员上限（含群主），默认 500
}

// MessageConfig 消息设置。
type MessageConfig struct {
	RecallSeconds int `yaml:"recall_seconds"` // 发送后多少秒内可撤回，默认 120
}

func (m MessageConfig) RecallWindow() time.Duration {
	return time.Duration(m.RecallSeconds) * time.Second
}

// StorageConfig S3 兼容存储（推荐 Garage，也可用 MinIO / 其他 S3）。用于语音消息，后续图片也走这里。
type StorageConfig struct {
	EnabledFlag       *bool    `yaml:"enabled"` // 可省略：填了 endpoint 即启用；显式 false 关闭语音
	Enabled           bool     `yaml:"-"`
	Endpoint          string   `yaml:"endpoint"`            // 服务端访问存储的地址（host:port，不带协议），如 s3.fansa.ccwu.cc:443 或 10.0.0.5:3900
	UseSSL            bool     `yaml:"use_ssl"`             // 服务端访问 endpoint 是否用 https
	PublicEndpoint    string   `yaml:"public_endpoint"`     // 浏览器访问的地址（预签名 URL 用），如 https://s3-im.fansa.ccwu.cc
	AccessKey         string   `yaml:"access_key"`          //
	SecretKey         string   `yaml:"secret_key"`          //
	Bucket            string   `yaml:"bucket"`              // 自动创建
	Region            string   `yaml:"region"`              // 必须与存储端一致：Garage 默认 garage，MinIO 默认 us-east-1
	CORSOrigins       []string `yaml:"cors_origins"`        // 启动时设置 bucket CORS 允许的网页 Origin；留空则用 server.allow_origins，再空则 *
	VoiceTTLDays      int      `yaml:"voice_ttl_days"`      // 服务器保留语音的天数（bucket 生命周期规则），默认 7
	SetLifecycle      bool     `yaml:"set_lifecycle"`       // 启动时自动设置生命周期规则和 CORS（access key 需要 owner 权限）
	VoiceMaxBytes     int64    `yaml:"voice_max_bytes"`     // 单条语音大小上限，默认 2MB
	VoiceMaxSeconds   int      `yaml:"voice_max_seconds"`   // 单条语音时长上限，默认 60
	PresignPutSeconds int      `yaml:"presign_put_seconds"` // 上传链接有效期，默认 300
	PresignGetSeconds int      `yaml:"presign_get_seconds"` // 下载链接有效期，默认 300
	VerifyUpload      bool     `yaml:"verify_upload"`       // 发送语音/图片时服务端检查对象（大小+文件头，2 次请求），默认 true；跨地域可关
	ImageMaxBytes     int64    `yaml:"image_max_bytes"`     // v0.3.0：单张图片大小上限，默认 10MB
	ImageTTLDays      int      `yaml:"image_ttl_days"`      // v0.3.0：服务器保留图片的天数，默认 0 = 永久（不加生命周期规则）
	FileMaxBytes      int64    `yaml:"file_max_bytes"`      // v0.4.0：单个文件大小上限，默认 100MB
	FileTTLDays       int      `yaml:"file_ttl_days"`       // v0.4.0：服务器保留文件的天数，默认 0 = 永久
	VideoMaxBytes     int64    `yaml:"video_max_bytes"`     // v0.4.0：单个视频大小上限，默认 100MB
	VideoMaxSeconds   int      `yaml:"video_max_seconds"`   // v0.4.0：单个视频时长上限，默认 600 秒
	VideoTTLDays      int      `yaml:"video_ttl_days"`      // v0.4.0：服务器保留视频的天数，默认 0 = 永久
	AvatarMaxBytes    int64    `yaml:"avatar_max_bytes"`    // v0.4.0：头像大小上限，默认 2MB
}

// STTConfig 语音转文字（v0.3.0）。默认关闭。
type STTConfig struct {
	Enabled           bool          `yaml:"enabled"`
	Provider          string        `yaml:"provider"`             // groq | cloudflare
	Fallback          string        `yaml:"fallback"`             // 可留空；主提供方 5xx/超时/429 时尝试
	Language          string        `yaml:"language"`             // 可留空让模型自动识别
	DailyLimitPerUser int           `yaml:"daily_limit_per_user"` // 每用户每天（Asia/Shanghai 日界）次数，0 = 不限
	TimeoutSeconds    int           `yaml:"timeout_seconds"`      // 单次请求提供方的超时
	Groq              GroqSTT       `yaml:"groq"`
	Cloudflare        CloudflareSTT `yaml:"cloudflare"`
}

type GroqSTT struct {
	APIKey  string `yaml:"api_key"`
	Model   string `yaml:"model"`
	BaseURL string `yaml:"base_url"` // 默认 https://api.groq.com/openai/v1（测试/代理时可改）
}

type CloudflareSTT struct {
	AccountID string `yaml:"account_id"`
	APIToken  string `yaml:"api_token"`
	Model     string `yaml:"model"`
	BaseURL   string `yaml:"base_url"` // 默认 https://api.cloudflare.com/client/v4（测试/代理时可改）
}

const (
	DefaultGroqBaseURL       = "https://api.groq.com/openai/v1"
	DefaultCloudflareBaseURL = "https://api.cloudflare.com/client/v4"
)

// Configured 该提供方的凭据是否已填写。
func (s STTConfig) Configured(provider string) bool {
	switch provider {
	case "groq":
		return s.Groq.APIKey != ""
	case "cloudflare":
		return s.Cloudflare.AccountID != "" && s.Cloudflare.APIToken != ""
	}
	return false
}

// CallsConfig 音视频通话：1 对 1 走 WebRTC P2P + Cloudflare Realtime TURN（v0.4.0），
// 多人通话走 Cloudflare Realtime SFU（v0.5.0）。两者默认关闭、凭据互相独立。
type CallsConfig struct {
	Enabled              bool   `yaml:"enabled"`
	RingTimeoutSeconds   int    `yaml:"ring_timeout_seconds"`   // 响铃超时（秒），默认 60
	CredentialTTLSeconds int    `yaml:"credential_ttl_seconds"` // TURN 临时凭证有效期（秒），默认 14400
	TurnKeyID            string `yaml:"turn_key_id"`            // Cloudflare TURN Key ID
	TurnAPIToken         string `yaml:"turn_api_token"`         // Cloudflare TURN API Token（只存服务端）
	BaseURL              string `yaml:"base_url"`               // 默认 https://rtc.live.cloudflare.com/v1（测试/代理时可改）
	// v0.5.0 多人通话（SFU）
	GroupEnabled         bool   `yaml:"group_enabled"`          // 多人通话总开关
	GroupMaxParticipants int    `yaml:"group_max_participants"` // 单房间人数上限（2–16），默认 9
	SFUAppID             string `yaml:"sfu_app_id"`             // Cloudflare Realtime(Calls) 应用 ID
	SFUAppSecret         string `yaml:"sfu_app_secret"`         // Cloudflare Realtime(Calls) 应用密钥（只存服务端）
}

const DefaultCallsBaseURL = "https://rtc.live.cloudflare.com/v1"

// Configured TURN 凭据是否已填写。
func (c CallsConfig) Configured() bool { return c.TurnKeyID != "" && c.TurnAPIToken != "" }

// GroupConfigured SFU（Calls 应用）凭据是否已填写。与 TURN 凭据是两套，不通用。
func (c CallsConfig) GroupConfigured() bool { return c.SFUAppID != "" && c.SFUAppSecret != "" }

// SMTPConfig 重置密码发信（v0.4.0）。默认关闭。
type SMTPConfig struct {
	Enabled          bool   `yaml:"enabled"`
	Host             string `yaml:"host"`                // 如 smtp.example.com
	Port             int    `yaml:"port"`                // 默认 587
	Username         string `yaml:"username"`            // 可留空（不做 AUTH）
	Password         string `yaml:"password"`            //
	From             string `yaml:"from"`                // 发件邮箱，如 no-reply@example.com
	TLSMode          string `yaml:"tls_mode"`            // starttls | tls | none，默认 starttls
	ResetURLBase     string `yaml:"reset_url_base"`      // 网页端公开地址（拼重置链接用），如 https://im.fansa.ccwu.cc
	TokenTTLMinutes  int    `yaml:"token_ttl_minutes"`   // 重置链接有效期（分钟），默认 30
	RateLimitPerHour int    `yaml:"rate_limit_per_hour"` // 同一邮箱每小时最多发几封（0 = 不限），默认 5
}

// Configured 发信必需配置（host/from/reset_url_base）是否已填写。
func (s SMTPConfig) Configured() bool {
	return s.Host != "" && s.From != "" && s.ResetURLBase != ""
}

// PublicURL 解析 public_endpoint，返回 host 和是否 https。
func (s StorageConfig) PublicURL() (host string, secure bool, err error) {
	pe := s.PublicEndpoint
	if pe == "" {
		return s.Endpoint, s.UseSSL, nil
	}
	if !strings.Contains(pe, "://") {
		return pe, s.UseSSL, nil
	}
	u, err := url.Parse(pe)
	if err != nil || u.Host == "" {
		return "", false, fmt.Errorf("storage.public_endpoint 格式错误: %q", pe)
	}
	if u.Path != "" && u.Path != "/" {
		return "", false, fmt.Errorf("storage.public_endpoint 不能带路径（S3 签名包含路径，请用独立子域名）")
	}
	return u.Host, u.Scheme == "https", nil
}

type LogConfig struct {
	Level string `yaml:"level"` // debug | info | warn | error
}

func (m MySQLConfig) DSN() string {
	return fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?charset=utf8mb4&collation=utf8mb4_unicode_ci&parseTime=true&loc=UTC",
		m.User, m.Password, m.Host, m.Port, m.Database)
}

func (j JWTConfig) TTL() time.Duration { return time.Duration(j.TTLHours) * time.Hour }

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件失败: %w", err)
	}
	cfg := &Config{
		Server:  ServerConfig{Addr: ":8092", NodeID: "node-1"},
		MySQL:   MySQLConfig{Port: 3306, MaxOpenConns: 50, MaxIdleConns: 10, ConnMaxLifetime: 300, AutoMigrate: true},
		Redis:   RedisConfig{Addr: "127.0.0.1:6379"},
		JWT:     JWTConfig{TTLHours: 24 * 30},
		Group:   GroupConfig{MaxMembers: 500},
		Message: MessageConfig{RecallSeconds: 120},
		Storage: StorageConfig{
			Bucket: "imchat", Region: "garage", VoiceTTLDays: 7, SetLifecycle: true,
			VoiceMaxBytes: 2 << 20, VoiceMaxSeconds: 60, PresignPutSeconds: 300, PresignGetSeconds: 300, VerifyUpload: true,
			ImageMaxBytes: 10 << 20, ImageTTLDays: 0,
			FileMaxBytes: 100 << 20, FileTTLDays: 0,
			VideoMaxBytes: 100 << 20, VideoMaxSeconds: 600, VideoTTLDays: 0,
			AvatarMaxBytes: 2 << 20,
		},
		Calls: CallsConfig{
			Enabled: false, RingTimeoutSeconds: 60, CredentialTTLSeconds: 14400, BaseURL: DefaultCallsBaseURL,
			GroupEnabled: false, GroupMaxParticipants: 9,
		},
		SMTP: SMTPConfig{
			Enabled: false, Port: 587, TLSMode: "starttls", TokenTTLMinutes: 30, RateLimitPerHour: 5,
		},
		STT: STTConfig{
			Enabled: false, Provider: "groq", Fallback: "", Language: "zh", DailyLimitPerUser: 50, TimeoutSeconds: 30,
			Groq:       GroqSTT{Model: "whisper-large-v3-turbo", BaseURL: DefaultGroqBaseURL},
			Cloudflare: CloudflareSTT{Model: "@cf/openai/whisper-large-v3-turbo", BaseURL: DefaultCloudflareBaseURL},
		},
		Log: LogConfig{Level: "info"},
	}
	if err := yaml.Unmarshal(b, cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件失败: %w", err)
	}
	if cfg.MySQL.Host == "" || cfg.MySQL.User == "" || cfg.MySQL.Database == "" {
		return nil, fmt.Errorf("mysql.host / mysql.user / mysql.database 不能为空")
	}
	if len(cfg.JWT.Secret) < 32 {
		return nil, fmt.Errorf("jwt.secret 至少 32 个字符")
	}
	if cfg.Group.MaxMembers < 3 {
		cfg.Group.MaxMembers = 3
	}
	if cfg.Message.RecallSeconds < 0 {
		cfg.Message.RecallSeconds = 0
	}
	cfg.Storage.Enabled = cfg.Storage.Endpoint != ""
	if cfg.Storage.EnabledFlag != nil {
		cfg.Storage.Enabled = *cfg.Storage.EnabledFlag
	}
	if s := &cfg.Storage; s.Enabled {
		if s.Endpoint == "" || s.Bucket == "" || s.AccessKey == "" || s.SecretKey == "" {
			return nil, fmt.Errorf("storage.endpoint / bucket / access_key / secret_key 不能为空（或设置 storage.enabled: false）")
		}
		if strings.Contains(s.Endpoint, "://") {
			return nil, fmt.Errorf("storage.endpoint 不要带 http(s)://，用 use_ssl 控制")
		}
		if _, _, err := s.PublicURL(); err != nil {
			return nil, err
		}
		if len(s.CORSOrigins) == 0 {
			s.CORSOrigins = cfg.Server.AllowOrigins
		}
		if s.Region == "" {
			s.Region = "garage"
		}
		if s.VoiceTTLDays < 1 {
			s.VoiceTTLDays = 7
		}
		if s.VoiceMaxBytes <= 0 {
			s.VoiceMaxBytes = 2 << 20
		}
		if s.VoiceMaxSeconds <= 0 {
			s.VoiceMaxSeconds = 60
		}
		if s.PresignPutSeconds <= 0 {
			s.PresignPutSeconds = 300
		}
		if s.PresignGetSeconds <= 0 {
			s.PresignGetSeconds = 300
		}
		if s.ImageMaxBytes <= 0 {
			s.ImageMaxBytes = 10 << 20
		}
		if s.ImageTTLDays < 0 {
			s.ImageTTLDays = 0
		}
		if s.FileMaxBytes <= 0 {
			s.FileMaxBytes = 100 << 20
		}
		if s.FileTTLDays < 0 {
			s.FileTTLDays = 0
		}
		if s.VideoMaxBytes <= 0 {
			s.VideoMaxBytes = 100 << 20
		}
		if s.VideoMaxSeconds <= 0 {
			s.VideoMaxSeconds = 600
		}
		if s.VideoTTLDays < 0 {
			s.VideoTTLDays = 0
		}
		if s.AvatarMaxBytes <= 0 {
			s.AvatarMaxBytes = 2 << 20
		}
	}
	if t := &cfg.STT; t.Enabled {
		t.Provider = strings.ToLower(strings.TrimSpace(t.Provider))
		t.Fallback = strings.ToLower(strings.TrimSpace(t.Fallback))
		if t.Provider == "" {
			t.Provider = "groq"
		}
		for _, p := range []string{t.Provider, t.Fallback} {
			if p != "" && p != "groq" && p != "cloudflare" {
				return nil, fmt.Errorf("stt.provider / stt.fallback 只能是 groq 或 cloudflare，当前 %q", p)
			}
		}
		if !t.Configured(t.Provider) {
			return nil, fmt.Errorf("stt.enabled=true 但主提供方 %s 未配置凭据（groq.api_key 或 cloudflare.account_id/api_token）", t.Provider)
		}
		if t.Fallback == t.Provider {
			t.Fallback = ""
		}
		if t.TimeoutSeconds <= 0 {
			t.TimeoutSeconds = 30
		}
		if t.DailyLimitPerUser < 0 {
			t.DailyLimitPerUser = 0
		}
		if t.Groq.Model == "" {
			t.Groq.Model = "whisper-large-v3-turbo"
		}
		if t.Cloudflare.Model == "" {
			t.Cloudflare.Model = "@cf/openai/whisper-large-v3-turbo"
		}
		t.Groq.BaseURL = strings.TrimRight(t.Groq.BaseURL, "/")
		if t.Groq.BaseURL == "" {
			t.Groq.BaseURL = DefaultGroqBaseURL
		}
		t.Cloudflare.BaseURL = strings.TrimRight(t.Cloudflare.BaseURL, "/")
		if t.Cloudflare.BaseURL == "" {
			t.Cloudflare.BaseURL = DefaultCloudflareBaseURL
		}
	}
	if c := &cfg.Calls; c.Enabled || c.GroupEnabled {
		if c.RingTimeoutSeconds <= 0 {
			c.RingTimeoutSeconds = 60
		}
		if c.CredentialTTLSeconds <= 0 {
			c.CredentialTTLSeconds = 14400
		}
		c.BaseURL = strings.TrimRight(c.BaseURL, "/")
		if c.BaseURL == "" {
			c.BaseURL = DefaultCallsBaseURL
		}
	}
	if c := &cfg.Calls; c.Enabled && !c.Configured() {
		return nil, fmt.Errorf("calls.enabled=true 但未配置凭据（turn_key_id / turn_api_token）")
	}
	if c := &cfg.Calls; c.GroupEnabled {
		if !c.GroupConfigured() {
			return nil, fmt.Errorf("calls.group_enabled=true 但未配置凭据（sfu_app_id / sfu_app_secret）")
		}
		if c.GroupMaxParticipants < 2 || c.GroupMaxParticipants > 16 {
			return nil, fmt.Errorf("calls.group_max_participants 必须在 2~16 之间，当前 %d", c.GroupMaxParticipants)
		}
	}
	if m := &cfg.SMTP; m.Enabled {
		if !m.Configured() {
			return nil, fmt.Errorf("smtp.enabled=true 但 smtp.host / smtp.from / smtp.reset_url_base 未填全")
		}
		m.TLSMode = strings.ToLower(strings.TrimSpace(m.TLSMode))
		if m.TLSMode == "" {
			m.TLSMode = "starttls"
		}
		if m.TLSMode != "starttls" && m.TLSMode != "tls" && m.TLSMode != "none" {
			return nil, fmt.Errorf("smtp.tls_mode 只能是 starttls / tls / none，当前 %q", m.TLSMode)
		}
		if m.Port <= 0 {
			m.Port = 587
		}
		if m.TokenTTLMinutes <= 0 {
			m.TokenTTLMinutes = 30
		}
		if m.RateLimitPerHour < 0 {
			m.RateLimitPerHour = 0
		}
		m.ResetURLBase = strings.TrimRight(m.ResetURLBase, "/")
	}
	return cfg, nil
}
