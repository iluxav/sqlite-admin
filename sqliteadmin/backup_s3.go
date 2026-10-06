package sqliteadmin

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// S3Config supports AWS S3 and compatible path-style APIs (MinIO, R2, etc.).
// Credentials stay on the server; they are never included in the UI.
type S3Config struct {
	Endpoint, Region, Bucket, Prefix           string
	AccessKeyID, SecretAccessKey, SessionToken string
}

type s3Store struct {
	cfg      S3Config
	endpoint *url.URL
	client   *http.Client
}

func newS3Store(cfg Config) (*s3Store, string) {
	s := cfg.S3
	if s == nil {
		s = &S3Config{Endpoint: os.Getenv("SQLITEADMIN_S3_ENDPOINT"), Region: os.Getenv("SQLITEADMIN_S3_REGION"),
			Bucket: os.Getenv("SQLITEADMIN_S3_BUCKET"), Prefix: os.Getenv("SQLITEADMIN_S3_PREFIX"),
			AccessKeyID: os.Getenv("SQLITEADMIN_S3_ACCESS_KEY_ID"), SecretAccessKey: os.Getenv("SQLITEADMIN_S3_SECRET_ACCESS_KEY"),
			SessionToken: os.Getenv("SQLITEADMIN_S3_SESSION_TOKEN")}
	}
	if *s == (S3Config{}) {
		return nil, ""
	}
	c := *s
	var missing []string
	for _, v := range []struct{ key, value string }{{"ENDPOINT", c.Endpoint}, {"BUCKET", c.Bucket}, {"ACCESS_KEY_ID", c.AccessKeyID}, {"SECRET_ACCESS_KEY", c.SecretAccessKey}} {
		if v.value == "" {
			missing = append(missing, "SQLITEADMIN_S3_"+v.key)
		}
	}
	if len(missing) != 0 {
		return nil, "S3 is unavailable. Missing: " + strings.Join(missing, ", ")
	}
	u, err := url.Parse(c.Endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, "S3 endpoint must be an http(s) URL without credentials, query, or fragment."
	}
	if strings.ContainsAny(c.Bucket, "/\\ \t\r\n") {
		return nil, "S3 bucket must be a bucket name, not a path."
	}
	if c.Region == "" {
		c.Region = "us-east-1"
	}
	if c.Prefix == "" {
		c.Prefix = "sqliteadmin/" + filepath.Base(cfg.Path)
	}
	c.Prefix = strings.Trim(c.Prefix, "/") + "/"
	return &s3Store{cfg: c, endpoint: u, client: &http.Client{Timeout: cfg.BackupTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, ""
}

func awsEscape(s string) string { return strings.ReplaceAll(url.QueryEscape(s), "+", "%20") }
func awsPath(s string) string {
	parts := strings.Split(s, "/")
	for i := range parts {
		parts[i] = awsEscape(parts[i])
	}
	return strings.Join(parts, "/")
}
func hashHex(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
func hmacSum(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

// AWS Signature V4: canonical URI, query and signed headers use the same
// bytes as the outgoing request. See AWS's single-chunk signature reference.
func signS3(r *http.Request, cfg S3Config, payload string, now time.Time) {
	now = now.UTC()
	r.Header.Set("X-Amz-Date", now.Format("20060102T150405Z"))
	r.Header.Set("X-Amz-Content-Sha256", payload)
	if cfg.SessionToken != "" {
		r.Header.Set("X-Amz-Security-Token", cfg.SessionToken)
	}
	headers := map[string]string{"host": r.URL.Host}
	if r.Host != "" {
		headers["host"] = r.Host
	}
	for k, values := range r.Header {
		name := strings.ToLower(k)
		if strings.HasPrefix(name, "x-amz-") || name == "range" {
			headers[name] = strings.Join(strings.Fields(strings.Join(values, ",")), " ")
		}
	}
	keys := make([]string, 0, len(headers))
	for k := range headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var canonical strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&canonical, "%s:%s\n", k, headers[k])
	}
	signed := strings.Join(keys, ";")
	r.URL.RawQuery = strings.ReplaceAll(r.URL.Query().Encode(), "+", "%20")
	request := strings.Join([]string{r.Method, r.URL.EscapedPath(), r.URL.RawQuery, canonical.String(), signed, payload}, "\n")
	day := now.Format("20060102")
	scope := day + "/" + cfg.Region + "/s3/aws4_request"
	toSign := "AWS4-HMAC-SHA256\n" + r.Header.Get("X-Amz-Date") + "\n" + scope + "\n" + hashHex([]byte(request))
	key := hmacSum([]byte("AWS4"+cfg.SecretAccessKey), day)
	key = hmacSum(key, cfg.Region)
	key = hmacSum(key, "s3")
	key = hmacSum(key, "aws4_request")
	r.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+cfg.AccessKeyID+"/"+scope+", SignedHeaders="+signed+", Signature="+hex.EncodeToString(hmacSum(key, toSign)))
}

func (s *s3Store) request(ctx context.Context, method, name string, query url.Values, body io.Reader, length int64, digest string) (*http.Response, error) {
	u := *s.endpoint
	u.Path = strings.TrimRight(u.Path, "/") + "/" + s.cfg.Bucket
	if name != "" {
		u.Path += "/" + s.cfg.Prefix + name
	}
	u.RawPath = awsPath(u.Path)
	u.RawQuery = query.Encode()
	r, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		r.ContentLength = length
		r.Header.Set("Content-Type", "application/vnd.sqlite3")
	}
	signS3(r, s.cfg, digest, time.Now())
	resp, err := s.client.Do(r)
	if err != nil {
		return nil, fmt.Errorf("S3 request failed: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		resp.Body.Close()
		// Do not surface provider response bodies; they may echo credentials.
		return nil, fmt.Errorf("S3 returned HTTP %d; check the endpoint, region, bucket and permissions", resp.StatusCode)
	}
	return resp, nil
}

func (s *s3Store) put(ctx context.Context, name, file string) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return err
	}
	if n > 5<<30 {
		return errors.New("S3 single-object uploads are limited to 5 GiB")
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	resp, err := s.request(ctx, "PUT", name, nil, f, n, hex.EncodeToString(h.Sum(nil)))
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (s *s3Store) get(ctx context.Context, name string, dst io.Writer, max int64) error {
	resp, err := s.request(ctx, "GET", name, nil, nil, 0, hashHex(nil))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	n, err := io.Copy(dst, io.LimitReader(resp.Body, max+1))
	if err != nil {
		return err
	}
	if n > max {
		return fmt.Errorf("backup exceeds the %d-byte restore limit", max)
	}
	return nil
}

func (s *s3Store) list(ctx context.Context) ([]backupEntry, error) {
	var out []backupEntry
	seen := map[string]bool{}
	token := ""
	for {
		q := url.Values{"list-type": {"2"}, "prefix": {s.cfg.Prefix}, "encoding-type": {"url"}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		resp, err := s.request(ctx, "GET", "", q, nil, 0, hashHex(nil))
		if err != nil {
			return nil, err
		}
		var page struct {
			Truncated bool   `xml:"IsTruncated"`
			Next      string `xml:"NextContinuationToken"`
			Encoding  string `xml:"EncodingType"`
			Contents  []struct {
				Key          string
				Size         int64
				LastModified time.Time
			}
		}
		err = xml.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&page)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("invalid S3 listing: %w", err)
		}
		for _, obj := range page.Contents {
			key := obj.Key
			if page.Encoding == "url" {
				key, err = url.PathUnescape(key)
				if err != nil {
					return nil, err
				}
			}
			if !strings.HasPrefix(key, s.cfg.Prefix) {
				continue
			}
			name := strings.TrimPrefix(key, s.cfg.Prefix)
			if validBackupName(name) {
				out = append(out, backupEntry{Name: name, Source: "s3", Size: obj.Size, Created: obj.LastModified})
			}
		}
		if !page.Truncated {
			return out, nil
		}
		if page.Next == "" || seen[page.Next] {
			return nil, errors.New("S3 returned an invalid pagination token")
		}
		seen[page.Next], token = true, page.Next
	}
}
