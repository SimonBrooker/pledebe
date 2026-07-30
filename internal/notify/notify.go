// Package notify sends email when pledebe finds something wrong.
//
// The hard part is not sending mail, it is not sending it repeatedly. Metrics
// are collected every fifteen minutes, so a naive implementation would email
// about the same corrupt database ninety-six times a day and be filtered to
// spam by lunchtime. Notifications therefore fire on CHANGE: once when a
// problem appears, once when it clears.
package notify

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/smtp"
	"strings"
	"time"
)

// Config describes where to send mail. Empty Host disables notification
// entirely, which is the default.
type Config struct {
	Host     string
	Port     int
	User     string
	Password string
	From     string
	To       []string

	// BaseURL, when set, is included in the message so the reader can open the
	// status page from the email. pledebe cannot infer its own external
	// address, so this is operator-supplied or absent.
	BaseURL string
}

// Enabled reports whether enough is configured to send.
func (c Config) Enabled() bool {
	return c.Host != "" && c.From != "" && len(c.To) > 0
}

// Validate explains what is missing, so a half-configured setup fails at
// startup with a clear message rather than silently never sending.
func (c Config) Validate() error {
	if c.Host == "" && c.From == "" && len(c.To) == 0 {
		return nil // not configured at all, which is fine
	}
	var missing []string
	if c.Host == "" {
		missing = append(missing, "SMTP_HOST")
	}
	if c.From == "" {
		missing = append(missing, "SMTP_FROM")
	}
	if len(c.To) == 0 {
		missing = append(missing, "SMTP_TO")
	}
	if len(missing) > 0 {
		return fmt.Errorf("email notification is partly configured; missing %s",
			strings.Join(missing, ", "))
	}
	return nil
}

// Send delivers one message.
//
// Handles both common SMTP shapes: implicit TLS on 465, and STARTTLS on 587 or
// 25. Home setups use all three, and getting this wrong looks like a silent
// failure rather than an error.
func (c Config) Send(subject, body string) error {
	if !c.Enabled() {
		return nil
	}

	port := c.Port
	if port == 0 {
		port = 587
	}
	addr := net.JoinHostPort(c.Host, fmt.Sprint(port))
	msg := c.message(subject, body)

	auth := newAuth(c.User, c.Password, c.Host)

	if port == 465 {
		return c.sendImplicitTLS(addr, auth, msg)
	}
	// smtp.SendMail upgrades to STARTTLS when the server advertises it.
	return smtp.SendMail(addr, auth, c.From, c.To, msg)
}

// sendImplicitTLS handles port 465, where TLS starts before any SMTP is spoken
// and smtp.SendMail therefore cannot be used.
func (c Config) sendImplicitTLS(addr string, auth smtp.Auth, msg []byte) error {
	conn, err := tls.Dial("tcp", addr, &tls.Config{ServerName: c.Host})
	if err != nil {
		return fmt.Errorf("tls connect: %w", err)
	}
	defer conn.Close()

	client, err := smtp.NewClient(conn, c.Host)
	if err != nil {
		return fmt.Errorf("smtp handshake: %w", err)
	}
	defer client.Quit()

	if auth != nil {
		if err := client.Auth(auth); err != nil {
			return fmt.Errorf("smtp auth: %w", err)
		}
	}
	if err := client.Mail(c.From); err != nil {
		return fmt.Errorf("smtp from: %w", err)
	}
	for _, to := range c.To {
		if err := client.Rcpt(to); err != nil {
			return fmt.Errorf("smtp to %s: %w", to, err)
		}
	}
	w, err := client.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	return w.Close()
}

// message assembles an RFC 5322 message.
func (c Config) message(subject, body string) []byte {
	var b strings.Builder

	fmt.Fprintf(&b, "From: %s\r\n", sanitiseHeader(c.From))
	fmt.Fprintf(&b, "To: %s\r\n", sanitiseHeader(strings.Join(c.To, ", ")))
	fmt.Fprintf(&b, "Subject: %s\r\n", sanitiseHeader(subject))
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("\r\n")
	b.WriteString(strings.ReplaceAll(body, "\n", "\r\n"))

	return []byte(b.String())
}

// sanitiseHeader strips CR and LF.
//
// Finding titles reach the subject line, and they include values read from the
// Plex database and its logs. A newline in a header lets an attacker inject
// arbitrary headers or a second message body.
func sanitiseHeader(s string) string {
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(s)
}
