// Package mail is the e-mail hub: named providers that plugins hand messages to.
package mail

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const maxMessageBytes = 512 * 1024

// Message is one e-mail to one recipient. The provider that delivers it supplies the sender identity.
type Message struct {
	// ID is stable per logical message; the Message-ID header derives from it, so a resend is recognisable.
	ID      string
	To      string
	Subject string
	Text    string
	HTML    string
	// Purpose labels the message in the usage counts (job, transfer, password, *-test …).
	Purpose string
}

// Provider delivers messages for plugins. Implementations must be safe for concurrent use.
type Provider interface {
	Send(ctx context.Context, m Message) error
}

// Transport performs one SMTP submission; msg is a complete CRLF message, dot-stuffing is the transport's job.
type Transport interface {
	Send(ctx context.Context, cfg SMTPConfig, password string, from string, to []string, msg []byte) error
}

// Error carries a fixed, non-sensitive failure kind; Permanent marks rejections a retry cannot fix.
type Error struct {
	Kind      string
	Permanent bool
}

// Error returns the failure kind, such as "smtp_auth_failed".
func (e *Error) Error() string { return e.Kind }

// RecipientRejected reports a permanent refusal of this recipient, which no other provider would accept either.
func RecipientRejected(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Permanent && (e.Kind == "smtp_recipient_rejected" || e.Kind == "invalid_mail_address")
}

var (
	localPart     = regexp.MustCompile(`^[A-Za-z0-9.!#$%&'*+/=?^_` + "`" + `{|}~-]+$`)
	hostLabel     = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)
	dottedIPv4    = regexp.MustCompile(`^\d+(?:\.\d+){3}$`)
	privateSuffix = regexp.MustCompile(`(?i)\.(?:localhost|local|internal|lan|home|test)$`)
	numericLabel  = regexp.MustCompile(`(?i)^(?:[0-9]+|0x[0-9a-f]*)$`)
	atext         = regexp.MustCompile(`^[A-Za-z0-9!#$%&'*+/=?^_` + "`" + `{|}~ -]*$`)
)

// ValidHost reports whether host is a public DNS name usable as an SMTP endpoint or mail domain.
func ValidHost(host string) bool {
	if len(host) > 253 || !strings.Contains(host, ".") || strings.EqualFold(host, "localhost") || dottedIPv4.MatchString(host) || privateSuffix.MatchString(host) {
		return false
	}
	labels := strings.Split(host, ".")
	for _, l := range labels {
		if !hostLabel.MatchString(l) {
			return false
		}
	}
	// URL parsers read a numeric final label as an IPv4 literal (127.1, 0x7f.1).
	return !numericLabel.MatchString(labels[len(labels)-1])
}

// ValidAddress reports whether v is an acceptable single recipient or sender address.
func ValidAddress(v string) bool {
	if len(v) > 254 {
		return false
	}
	for i := 0; i < len(v); i++ {
		if v[i] < 0x21 || v[i] > 0x7e {
			return false
		}
	}
	at := strings.LastIndexByte(v, '@')
	if at < 1 || at > 64 {
		return false
	}
	local := v[:at]
	return localPart.MatchString(local) && !strings.HasPrefix(local, ".") && !strings.HasSuffix(local, ".") &&
		!strings.Contains(local, "..") && ValidHost(v[at+1:])
}

func ascii(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// encodedWords splits s into RFC 2047 B-encoded words of at most 42 UTF-8 bytes, folded onto continuation lines.
func encodedWords(s string) string {
	var words []string
	chunk := ""
	for _, r := range s {
		if len(chunk)+utf8.RuneLen(r) > 42 {
			words = append(words, "=?UTF-8?B?"+base64.StdEncoding.EncodeToString([]byte(chunk))+"?=")
			chunk = ""
		}
		chunk += string(r)
	}
	if chunk != "" {
		words = append(words, "=?UTF-8?B?"+base64.StdEncoding.EncodeToString([]byte(chunk))+"?=")
	}
	return strings.Join(words, "\r\n ")
}

func subjectHeader(s string) string {
	if ascii(s) && len(s) <= 900 {
		return s
	}
	return encodedWords(s)
}

func displayName(s string) string {
	if ascii(s) && atext.MatchString(s) {
		return s
	}
	if ascii(s) {
		return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
	}
	return encodedWords(s)
}

func base64Lines(s string) string {
	enc := base64.StdEncoding.EncodeToString([]byte(s))
	var b strings.Builder
	for len(enc) > 76 {
		b.WriteString(enc[:76])
		b.WriteString("\r\n")
		enc = enc[76:]
	}
	b.WriteString(enc)
	return b.String()
}

// BuildMessage renders a multipart/alternative message; every header value is validated or encoded, never raw input.
func BuildMessage(cfg SMTPConfig, m Message, now time.Time) ([]byte, error) {
	id, to := m.ID, m.To
	if !ValidAddress(cfg.FromAddress) || !ValidAddress(to) || (cfg.ReplyTo != "" && !ValidAddress(cfg.ReplyTo)) {
		return nil, &Error{Kind: "invalid_mail_address", Permanent: true}
	}
	if strings.ContainsAny(cfg.FromName+m.Subject, "\r\n\x00") {
		return nil, &Error{Kind: "invalid_mail_header", Permanent: true}
	}
	sum := sha256.Sum256([]byte(id))
	hash := hex.EncodeToString(sum[:])
	boundary := "go-ldap-admin-" + hash[:32]
	from := "<" + cfg.FromAddress + ">"
	if cfg.FromName != "" {
		from = displayName(cfg.FromName) + " " + from
	}
	h := []string{
		"Date: " + now.UTC().Format(time.RFC1123Z),
		"Message-ID: <" + hash + "@" + cfg.FromAddress[strings.LastIndexByte(cfg.FromAddress, '@')+1:] + ">",
		"From: " + from,
		"To: <" + to + ">",
		"Subject: " + subjectHeader(m.Subject),
	}
	if cfg.ReplyTo != "" {
		h = append(h, "Reply-To: <"+cfg.ReplyTo+">")
	}
	h = append(h, "MIME-Version: 1.0", `Content-Type: multipart/alternative; boundary="`+boundary+`"`)
	var b strings.Builder
	b.WriteString(strings.Join(h, "\r\n"))
	b.WriteString("\r\n\r\n")
	for _, p := range []struct{ typ, body string }{{"text/plain", m.Text}, {"text/html", m.HTML}} {
		b.WriteString("--" + boundary + "\r\nContent-Type: " + p.typ + "; charset=UTF-8\r\nContent-Transfer-Encoding: base64\r\n\r\n")
		b.WriteString(base64Lines(p.body))
		b.WriteString("\r\n")
	}
	b.WriteString("--" + boundary + "--\r\n")
	if b.Len() > maxMessageBytes {
		return nil, &Error{Kind: "mail_too_large", Permanent: true}
	}
	return []byte(b.String()), nil
}
