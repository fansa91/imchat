// Package mail 重置密码发信（v0.4.0）。只用标准库 net/smtp + crypto/tls，不加新依赖。
package mail

import (
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"strings"
	"time"

	"imserver/internal/config"
)

const sendTimeout = 10 * time.Second

// Sender 按 SMTP 配置发信。未启用或配置不全时 New 返回 nil。
type Sender struct {
	cfg config.SMTPConfig
}

func New(cfg config.SMTPConfig) *Sender {
	if !cfg.Enabled || !cfg.Configured() {
		return nil
	}
	return &Sender{cfg: cfg}
}

// Config 返回发信配置（调用方取 reset_url_base / token_ttl_minutes / rate_limit_per_hour）。
func (s *Sender) Config() config.SMTPConfig { return s.cfg }

// SendResetPassword 发送重置密码邮件：纯文本 + HTML 各一份，正文含重置链接与有效期。
func (s *Sender) SendResetPassword(to, link string) error {
	ttl := s.cfg.TokenTTLMinutes
	subject := "重置你的 IMChat 密码"
	text := fmt.Sprintf("你正在重置 IMChat 账号密码。\r\n\r\n请在 %d 分钟内打开下面的链接设置新密码：\r\n%s\r\n\r\n如果不是你本人操作，请忽略这封邮件，你的密码不会被修改。\r\n", ttl, link)
	html := fmt.Sprintf("<p>你正在重置 IMChat 账号密码。</p><p>请在 %d 分钟内打开下面的链接设置新密码：</p><p><a href=\"%s\">%s</a></p><p>如果不是你本人操作，请忽略这封邮件，你的密码不会被修改。</p>", ttl, link, link)
	return s.send(to, subject, text, html)
}

func (s *Sender) send(to, subject, textBody, htmlBody string) error {
	boundary := "imchat-" + base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprint(time.Now().UnixNano())))
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", s.cfg.From)
	fmt.Fprintf(&b, "To: %s\r\n", to)
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.BEncoding.Encode("utf-8", subject))
	b.WriteString("MIME-Version: 1.0\r\n")
	fmt.Fprintf(&b, "Content-Type: multipart/alternative; boundary=%q\r\n\r\n", boundary)
	fmt.Fprintf(&b, "--%s\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n%s\r\n", boundary, textBody)
	fmt.Fprintf(&b, "--%s\r\nContent-Type: text/html; charset=utf-8\r\n\r\n%s\r\n", boundary, htmlBody)
	fmt.Fprintf(&b, "--%s--\r\n", boundary)

	addr := net.JoinHostPort(s.cfg.Host, fmt.Sprint(s.cfg.Port))
	dialer := &net.Dialer{Timeout: sendTimeout}
	var conn net.Conn
	var err error
	if s.cfg.TLSMode == "tls" {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{ServerName: s.cfg.Host, MinVersion: tls.VersionTLS12})
	} else {
		conn, err = dialer.Dial("tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("连接 SMTP 服务器失败: %w", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(sendTimeout))
	cli, err := smtp.NewClient(conn, s.cfg.Host)
	if err != nil {
		return fmt.Errorf("SMTP 握手失败: %w", err)
	}
	defer cli.Close()
	if s.cfg.TLSMode == "starttls" {
		if ok, _ := cli.Extension("STARTTLS"); ok {
			if err := cli.StartTLS(&tls.Config{ServerName: s.cfg.Host, MinVersion: tls.VersionTLS12}); err != nil {
				return fmt.Errorf("SMTP STARTTLS 失败: %w", err)
			}
		}
	}
	if s.cfg.Username != "" {
		if err := cli.Auth(smtp.PlainAuth("", s.cfg.Username, s.cfg.Password, s.cfg.Host)); err != nil {
			return fmt.Errorf("SMTP 认证失败: %w", err)
		}
	}
	if err := cli.Mail(s.cfg.From); err != nil {
		return err
	}
	if err := cli.Rcpt(to); err != nil {
		return err
	}
	w, err := cli.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write([]byte(b.String())); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return cli.Quit()
}
