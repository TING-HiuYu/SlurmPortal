// Package objstore is a minimal S3 (SigV4) client for one private bucket, used against the
// Cloudflare R2 S3 API. It runs natively and on the Worker, where net/http is backed by fetch.
package objstore

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	unsignedPayload = "UNSIGNED-PAYLOAD"
	// MaxPresign is the SigV4 ceiling for presigned URL lifetime.
	MaxPresign = 7 * 24 * time.Hour
)

var (
	accountPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)
	bucketPattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	keyIDPattern   = regexp.MustCompile(`^[A-Za-z0-9]{16,128}$`)
	metaKeyPattern = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)
)

// Credentials are an S3 access key pair.
type Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
}

// Validate checks the key ID and secret look like R2/S3 credentials.
func (c Credentials) Validate() error {
	if !keyIDPattern.MatchString(c.AccessKeyID) || len(c.SecretAccessKey) < 32 || len(c.SecretAccessKey) > 128 || strings.ContainsAny(c.SecretAccessKey, "\r\n\x00") {
		return errors.New("objstore: invalid S3 credentials")
	}
	return nil
}

// Client addresses exactly one bucket with path-style URLs.
type Client struct {
	// Endpoint is scheme://host; for R2 it is https://<account>.r2.cloudflarestorage.com.
	Endpoint string
	Bucket   string
	Region   string
	Creds    Credentials
	HTTP     *http.Client
	Now      func() time.Time
}

// R2 returns a client for a Cloudflare R2 bucket.
func R2(account, bucket string, creds Credentials) (*Client, error) {
	if !accountPattern.MatchString(account) {
		return nil, errors.New("objstore: R2 account id must be 32 lowercase hex characters")
	}
	c := &Client{Endpoint: "https://" + account + ".r2.cloudflarestorage.com", Bucket: bucket, Region: "auto", Creds: creds}
	return c, c.Validate()
}

func (c *Client) Validate() error {
	u, err := url.Parse(c.Endpoint)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.User != nil {
		return errors.New("objstore: endpoint must be scheme://host")
	}
	if !bucketPattern.MatchString(c.Bucket) || strings.Contains(c.Bucket, "..") || c.Region == "" {
		return errors.New("objstore: invalid bucket or region")
	}
	return c.Creds.Validate()
}

// Object is the metadata HEAD returns.
type Object struct {
	ETag string
	Meta map[string]string
}

// ErrPrecondition reports a lost conditional write.
var ErrPrecondition = errors.New("objstore: precondition failed")

// StatusError is an unexpected S3 response status.
type StatusError struct {
	Op     string
	Status int
}

// Error names the operation and the HTTP status it got.
func (e *StatusError) Error() string {
	return fmt.Sprintf("objstore: %s returned HTTP %d", e.Op, e.Status)
}

// Head returns nil, nil when the object does not exist.
func (c *Client) Head(ctx context.Context, key string) (*Object, error) {
	resp, err := c.do(ctx, http.MethodHead, key, nil, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNotFound:
		return nil, nil
	case http.StatusOK:
	default:
		return nil, &StatusError{"HEAD", resp.StatusCode}
	}
	o := &Object{ETag: resp.Header.Get("ETag"), Meta: map[string]string{}}
	for k, v := range resp.Header {
		if name, ok := strings.CutPrefix(strings.ToLower(k), "x-amz-meta-"); ok && len(v) > 0 {
			o.Meta[name] = v[0]
		}
	}
	return o, nil
}

// Get returns the object body (at most max bytes), or nil, nil when it does not exist.
func (c *Client) Get(ctx context.Context, key string, max int64) ([]byte, error) {
	resp, err := c.do(ctx, http.MethodGet, key, nil, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNotFound:
		return nil, nil
	case http.StatusOK:
	default:
		return nil, &StatusError{"GET", resp.StatusCode}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > max {
		return nil, fmt.Errorf("objstore: %s is larger than %d bytes", key, max)
	}
	return body, nil
}

// PutOptions describe one PUT; exactly one of IfMatch or IfNoneMatch may be set.
type PutOptions struct {
	ContentType  string
	CacheControl string
	Meta         map[string]string
	IfMatch      string
	// IfNoneMatch creates the object only when it does not exist yet.
	IfNoneMatch bool
}

// Put writes body; a failed condition returns ErrPrecondition.
func (c *Client) Put(ctx context.Context, key string, body []byte, o PutOptions) error {
	if o.IfMatch != "" && o.IfNoneMatch {
		return errors.New("objstore: IfMatch and IfNoneMatch are exclusive")
	}
	h := http.Header{}
	if o.ContentType != "" {
		h.Set("Content-Type", o.ContentType)
	}
	if o.CacheControl != "" {
		h.Set("Cache-Control", o.CacheControl)
	}
	for k, v := range o.Meta {
		if !metaKeyPattern.MatchString(k) || strings.ContainsAny(v, "\r\n\x00") {
			return errors.New("objstore: invalid metadata")
		}
		h.Set("X-Amz-Meta-"+k, v)
	}
	if o.IfMatch != "" {
		h.Set("If-Match", o.IfMatch)
	}
	if o.IfNoneMatch {
		h.Set("If-None-Match", "*")
	}
	resp, err := c.do(ctx, http.MethodPut, key, h, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusPreconditionFailed, http.StatusConflict:
		return ErrPrecondition
	}
	return &StatusError{"PUT", resp.StatusCode}
}

// Delete removes key; deleting a missing key succeeds.
func (c *Client) Delete(ctx context.Context, key string) error {
	resp, err := c.do(ctx, http.MethodDelete, key, nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
		return &StatusError{"DELETE", resp.StatusCode}
	}
	return nil
}

func (c *Client) objectURL(key string) (*url.URL, error) {
	if key == "" || strings.HasPrefix(key, "/") || strings.Contains(key, "..") {
		return nil, errors.New("objstore: invalid key")
	}
	u, _ := url.Parse(c.Endpoint)
	u.Path = "/" + c.Bucket + "/" + key
	u.RawPath = "/" + uriEncode(c.Bucket, false) + "/" + uriEncode(key, true)
	return u, nil
}

func (c *Client) do(ctx context.Context, method, key string, h http.Header, body []byte) (*http.Response, error) {
	u, err := c.objectURL(key)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if body == nil {
		req.Body, req.ContentLength = http.NoBody, 0
	}
	for k, v := range h {
		req.Header[k] = v
	}
	sum := sha256.Sum256(body)
	payload := hex.EncodeToString(sum[:])
	req.Header.Set("X-Amz-Content-Sha256", payload)
	req.Header.Set("X-Amz-Date", c.now().Format("20060102T150405Z"))
	req.Header.Set("Authorization", c.authorization(req, payload))
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	return client.Do(req)
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}

func (c *Client) authorization(req *http.Request, payload string) string {
	stamp := req.Header.Get("X-Amz-Date")
	names := []string{"host"}
	headers := map[string]string{"host": req.URL.Host}
	for k, v := range req.Header {
		lk := strings.ToLower(k)
		if lk == "authorization" || lk == "user-agent" || lk == "content-length" {
			continue
		}
		names = append(names, lk)
		headers[lk] = strings.TrimSpace(strings.Join(v, ","))
	}
	sort.Strings(names)
	var canon strings.Builder
	for _, n := range names {
		canon.WriteString(n + ":" + headers[n] + "\n")
	}
	signed := strings.Join(names, ";")
	scope := stamp[:8] + "/" + c.Region + "/s3/aws4_request"
	creq := strings.Join([]string{req.Method, req.URL.EscapedPath(), canonicalQuery(req.URL.Query()), canon.String(), signed, payload}, "\n")
	sig := c.signature(stamp, scope, creq)
	return "AWS4-HMAC-SHA256 Credential=" + c.Creds.AccessKeyID + "/" + scope + ", SignedHeaders=" + signed + ", Signature=" + sig
}

// PresignGET returns a URL granting GET of exactly key until now+ttl.
func (c *Client) PresignGET(key string, ttl time.Duration) (string, time.Time, error) {
	return c.presign(http.MethodGet, key, ttl, c.now())
}

func (c *Client) presign(method, key string, ttl time.Duration, now time.Time) (string, time.Time, error) {
	secs := int64(ttl / time.Second)
	if secs < 1 || ttl > MaxPresign {
		return "", time.Time{}, errors.New("objstore: presign lifetime out of range")
	}
	u, err := c.objectURL(key)
	if err != nil {
		return "", time.Time{}, err
	}
	stamp := now.Format("20060102T150405Z")
	scope := stamp[:8] + "/" + c.Region + "/s3/aws4_request"
	q := url.Values{
		"X-Amz-Algorithm":     {"AWS4-HMAC-SHA256"},
		"X-Amz-Credential":    {c.Creds.AccessKeyID + "/" + scope},
		"X-Amz-Date":          {stamp},
		"X-Amz-Expires":       {fmt.Sprint(secs)},
		"X-Amz-SignedHeaders": {"host"},
	}
	query := canonicalQuery(q)
	creq := strings.Join([]string{method, u.EscapedPath(), query, "host:" + u.Host + "\n", "host", unsignedPayload}, "\n")
	u.RawQuery = query + "&X-Amz-Signature=" + c.signature(stamp, scope, creq)
	return u.String(), now.Add(time.Duration(secs) * time.Second), nil
}

func (c *Client) signature(stamp, scope, creq string) string {
	h := sha256.Sum256([]byte(creq))
	toSign := "AWS4-HMAC-SHA256\n" + stamp + "\n" + scope + "\n" + hex.EncodeToString(h[:])
	k := mac([]byte("AWS4"+c.Creds.SecretAccessKey), stamp[:8])
	k = mac(k, c.Region)
	k = mac(k, "s3")
	k = mac(k, "aws4_request")
	return hex.EncodeToString(mac(k, toSign))
}

func mac(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

func canonicalQuery(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vs := append([]string(nil), q[k]...)
		sort.Strings(vs)
		for _, v := range vs {
			parts = append(parts, uriEncode(k, false)+"="+uriEncode(v, false))
		}
	}
	return strings.Join(parts, "&")
}

// uriEncode is the SigV4 UriEncode: unreserved characters stay, everything else is %XX.
func uriEncode(s string, keepSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case 'A' <= ch && ch <= 'Z', 'a' <= ch && ch <= 'z', '0' <= ch && ch <= '9', ch == '-', ch == '_', ch == '.', ch == '~':
			b.WriteByte(ch)
		case ch == '/' && keepSlash:
			b.WriteByte(ch)
		default:
			fmt.Fprintf(&b, "%%%02X", ch)
		}
	}
	return b.String()
}
