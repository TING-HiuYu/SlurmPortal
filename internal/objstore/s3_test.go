package objstore

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/TING-HiuYu/SlurmPortal/internal/objstore/objstoretest"
)

var awsCreds = Credentials{AccessKeyID: "AKIAIOSFODNN7EXAMPLE", SecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"}

// Vectors from the AWS "Signature Version 4 examples" documentation (virtual-hosted style).
func TestSigV4Vectors(t *testing.T) {
	c := &Client{Region: "us-east-1", Creds: awsCreds}
	t.Run("header GET with range", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, "https://examplebucket.s3.amazonaws.com/test.txt", nil)
		req.Header.Set("Range", "bytes=0-9")
		req.Header.Set("X-Amz-Content-Sha256", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855")
		req.Header.Set("X-Amz-Date", "20130524T000000Z")
		got := c.authorization(req, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855")
		want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, SignedHeaders=host;range;x-amz-content-sha256;x-amz-date, Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
		if got != want {
			t.Fatalf("got %s", got)
		}
	})
	t.Run("presigned GET", func(t *testing.T) {
		q := url.Values{"X-Amz-Algorithm": {"AWS4-HMAC-SHA256"}, "X-Amz-Credential": {"AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request"},
			"X-Amz-Date": {"20130524T000000Z"}, "X-Amz-Expires": {"86400"}, "X-Amz-SignedHeaders": {"host"}}
		creq := strings.Join([]string{"GET", "/test.txt", canonicalQuery(q), "host:examplebucket.s3.amazonaws.com\n", "host", unsignedPayload}, "\n")
		if got := c.signature("20130524T000000Z", "20130524/us-east-1/s3/aws4_request", creq); got != "aeeed9bbccd4d02ee5c0109b86d86835f995330da4c265957d157751f604d404" {
			t.Fatalf("got %s", got)
		}
	})
}

func TestPresignGET(t *testing.T) {
	c, err := R2(strings.Repeat("a", 32), "status-bucket", Credentials{AccessKeyID: "ABCDEFGHIJKLMNOP", SecretAccessKey: strings.Repeat("s", 40)})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 30, 1, 2, 3, 0, time.UTC)
	c.Now = func() time.Time { return now }
	raw, exp, err := c.PresignGET("current-status/v1/cluster1/current.json", 15*time.Minute)
	if err != nil || !exp.Equal(now.Add(15*time.Minute)) {
		t.Fatal(raw, exp, err)
	}
	u, _ := url.Parse(raw)
	if u.Host != strings.Repeat("a", 32)+".r2.cloudflarestorage.com" || u.Path != "/status-bucket/current-status/v1/cluster1/current.json" ||
		u.Query().Get("X-Amz-Expires") != "900" || u.Query().Get("X-Amz-Credential") != "ABCDEFGHIJKLMNOP/20261030/auto/s3/aws4_request" || len(u.Query().Get("X-Amz-Signature")) != 64 {
		t.Fatal(raw)
	}
	for _, bad := range []time.Duration{0, MaxPresign + time.Second} {
		if _, _, err := c.PresignGET("k", bad); err == nil {
			t.Fatal("lifetime accepted", bad)
		}
	}
	for _, key := range []string{"", "/abs", "a/../b"} {
		if _, _, err := c.PresignGET(key, time.Minute); err == nil {
			t.Fatal("key accepted", key)
		}
	}
}

func TestValidate(t *testing.T) {
	good := Credentials{AccessKeyID: "ABCDEFGHIJKLMNOP", SecretAccessKey: strings.Repeat("s", 40)}
	if _, err := R2("XYZ", "bucket", good); err == nil {
		t.Fatal("bad account accepted")
	}
	if _, err := R2(strings.Repeat("a", 32), "B!", good); err == nil {
		t.Fatal("bad bucket accepted")
	}
	if _, err := R2(strings.Repeat("a", 32), "bucket", Credentials{AccessKeyID: "short", SecretAccessKey: "x"}); err == nil {
		t.Fatal("bad credentials accepted")
	}
}

func newFake(t *testing.T) (*Client, *objstoretest.Server) {
	c := &Client{Bucket: "bucket", Region: "auto", Creds: Credentials{AccessKeyID: "ABCDEFGHIJKLMNOP", SecretAccessKey: strings.Repeat("s", 40)}}
	f := objstoretest.New(t)
	f.Verify = func(r *http.Request) bool {
		check := r.Clone(context.Background())
		check.Header.Del("Authorization")
		check.Header.Del("Accept-Encoding")
		check.URL.Host = r.Host
		return r.Header.Get("Authorization") == c.authorization(check, r.Header.Get("X-Amz-Content-Sha256"))
	}
	c.Endpoint = f.URL
	return c, f
}

func TestHeadPutConditional(t *testing.T) {
	ctx := context.Background()
	c, f := newFake(t)
	key := "current-status/v1/cluster1/current.json"
	if o, err := c.Head(ctx, key); o != nil || err != nil {
		t.Fatal(o, err)
	}
	opts := PutOptions{ContentType: "application/json", Meta: map[string]string{"observed-ms": "1"}, IfNoneMatch: true}
	if err := c.Put(ctx, key, []byte(`{}`), opts); err != nil {
		t.Fatal(err)
	}
	if err := c.Put(ctx, key, []byte(`{}`), opts); !errors.Is(err, ErrPrecondition) {
		t.Fatal("create-only overwrote", err)
	}
	o, err := c.Head(ctx, key)
	if err != nil || o.Meta["observed-ms"] != "1" || o.ETag == "" {
		t.Fatal(o, err)
	}
	if err := c.Put(ctx, key, []byte(`{"a":1}`), PutOptions{IfMatch: `"stale"`}); !errors.Is(err, ErrPrecondition) {
		t.Fatal(err)
	}
	if err := c.Put(ctx, key, []byte(`{"a":1}`), PutOptions{IfMatch: o.ETag}); err != nil {
		t.Fatal(err)
	}
	if string(must(f.Object("/bucket/"+key)).Body) != `{"a":1}` {
		t.Fatal("body not replaced")
	}
	if err := c.Put(ctx, key, nil, PutOptions{IfMatch: "x", IfNoneMatch: true}); err == nil {
		t.Fatal("exclusive conditions accepted")
	}
	if err := c.Put(ctx, key, nil, PutOptions{Meta: map[string]string{"Bad Key": "v"}}); err == nil {
		t.Fatal("bad metadata accepted")
	}
}

func must(o objstoretest.Object, ok bool) objstoretest.Object { return o }
