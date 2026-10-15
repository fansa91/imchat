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
	return cfg, nil
}
