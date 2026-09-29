package daytona

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"
)

// contextDir is where the context files sit in the build context; the Dockerfile copies it
// into the working directory.
const contextDir = "casebox-context"

// contextTar packs the context files under contextDir. The archive is deterministic, so equal
// files give an equal hash and Daytona reuses the upload.
func contextTar(files map[string][]byte) ([]byte, error) {
	paths := make([]string, 0, len(files))
	for p := range files {
		if p == "" || path.IsAbs(p) || strings.Contains(p, "\\") || path.Clean(p) != p || p == ".." || strings.HasPrefix(p, "../") {
			return nil, fmt.Errorf("the context file %q is not a clean repository-relative path", p)
		}
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	for _, p := range paths {
		body := files[p]
		h := &tar.Header{Name: contextDir + "/" + p, Mode: 0o644, Size: int64(len(body)), ModTime: time.Unix(0, 0), Typeflag: tar.TypeReg}
		if err := w.WriteHeader(h); err != nil {
			return nil, err
		}
		if _, err := w.Write(body); err != nil {
			return nil, err
		}
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// uploadContext puts the context archive where Daytona's runner reads build contexts:
// <organizationId>/<hash>/context.tar in the bucket GET /object-storage/push-access names.
// It returns the hash to list in buildInfo.contextHashes.
func (p *Provider) uploadContext(ctx context.Context, archive []byte) (string, error) {
	sum := md5.Sum(archive)
	hash := hex.EncodeToString(sum[:])
	var access storageAccess
	if err := p.c.call(ctx, http.MethodGet, p.c.platform("/object-storage/push-access"), nil, &access); err != nil {
		return "", fmt.Errorf("get Daytona object storage access: %w", err)
	}
	endpoint := strings.TrimRight(access.StorageURL, "/")
	if !strings.Contains(endpoint, "://") {
		endpoint = "https://" + endpoint
	}
	target, err := url.Parse(endpoint + "/" + access.Bucket + "/" + access.OrganizationID + "/" + hash + "/context.tar")
	if err != nil {
		return "", fmt.Errorf("the Daytona storage URL %q is not valid: %w", access.StorageURL, err)
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, target.String(), bytes.NewReader(archive))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-tar")
	if access.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", access.SessionToken)
	}
	payload := sha256.Sum256(archive)
	signV4(req, hex.EncodeToString(payload[:]), access.AccessKey, access.Secret, storageRegion(endpoint), time.Now().UTC())
	resp, err := p.c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("upload the build context: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", fmt.Errorf("upload the build context: %w", errorOf(resp))
	}
	return hash, nil
}

// storageRegion follows Daytona's SDK: us-east-1, unless the endpoint is AWS S3 with a region.
func storageRegion(endpoint string) string {
	if strings.Contains(endpoint, "amazonaws.com") {
		parts := strings.Split(endpoint, ".")
		for i, part := range parts {
			if part == "s3" && i+1 < len(parts) {
				return parts[i+1]
			}
		}
	}
	return "us-east-1"
}

// signV4 signs req for S3 with AWS Signature Version 4, over the host and every header set on
// req. payloadHash is the hex SHA-256 of the body.
func signV4(req *http.Request, payloadHash, accessKey, secret, region string, now time.Time) {
	amzDate := now.Format("20060102T150405Z")
	day := now.Format("20060102")
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)

	headers := map[string]string{"host": req.URL.Host}
	for name, values := range req.Header {
		headers[strings.ToLower(name)] = strings.TrimSpace(strings.Join(values, ","))
	}
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)
	var canonicalHeaders strings.Builder
	for _, name := range names {
		canonicalHeaders.WriteString(name + ":" + headers[name] + "\n")
	}
	signedHeaders := strings.Join(names, ";")

	uri := req.URL.EscapedPath()
	if uri == "" {
		uri = "/"
	}
	canonicalRequest := strings.Join([]string{req.Method, uri, canonicalQuery(req.URL.Query()), canonicalHeaders.String(), signedHeaders, payloadHash}, "\n")
	scope := day + "/" + region + "/s3/aws4_request"
	requestHash := sha256.Sum256([]byte(canonicalRequest))
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(requestHash[:])

	key := hmacSHA256([]byte("AWS4"+secret), day)
	key = hmacSHA256(key, region)
	key = hmacSHA256(key, "s3")
	key = hmacSHA256(key, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(key, toSign))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+accessKey+"/"+scope+", SignedHeaders="+signedHeaders+", Signature="+signature)
}

func canonicalQuery(values url.Values) string {
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vs := append([]string(nil), values[k]...)
		sort.Strings(vs)
		for _, v := range vs {
			parts = append(parts, awsEscape(k)+"="+awsEscape(v))
		}
	}
	return strings.Join(parts, "&")
}

// awsEscape is RFC 3986 escaping as SigV4 wants it: only unreserved characters stay as they are.
func awsEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}
