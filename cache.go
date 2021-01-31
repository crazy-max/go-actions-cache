package actionscache

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"

	jwt "github.com/dgrijalva/jwt-go"
	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"
)

var UploadConcurrency = 4
var UploadChunkSize = 32 * 1024 * 1024

var Log = func(string, ...interface{}) {}

func TryEnv() (*Cache, error) {
	tokenEnc, ok := os.LookupEnv("GHCACHE_TOKEN_ENC")
	if ok {
		url, token, err := decryptToken(tokenEnc, os.Getenv("GHCACHE_TOKEN_PW"))
		if err != nil {
			return nil, err
		}
		return New(token, url)
	}

	token, ok := os.LookupEnv("ACTIONS_RUNTIME_TOKEN")
	if !ok {
		return nil, nil
	}

	// ACTIONS_CACHE_URL=https://artifactcache.actions.githubusercontent.com/xxx/
	cacheURL, ok := os.LookupEnv("ACTIONS_CACHE_URL")
	if !ok {
		return nil, nil
	}

	return New(token, cacheURL)
}

func New(token, url string) (*Cache, error) {
	tk, _, err := new(jwt.Parser).ParseUnverified(token, jwt.MapClaims{})
	if err != nil {
		return nil, errors.WithStack(err)
	}
	claims, ok := tk.Claims.(jwt.MapClaims)
	if !ok {
		return nil, errors.Errorf("invalid token without claims map")
	}
	ac, ok := claims["ac"]
	if !ok {
		return nil, errors.Errorf("invalid token without access controls")
	}
	acs, ok := ac.(string)
	if !ok {
		return nil, errors.Errorf("invalid token without access controls type")
	}

	scopes := []Scope{}
	if err := json.Unmarshal([]byte(acs), &scopes); err != nil {
		return nil, errors.Wrap(err, "failed to parse token access controls")
	}
	Log("parsed token: scopes %+v", scopes)

	return &Cache{
		scopes: scopes,
		URL:    url,
		Token:  tk,
	}, nil
}

type Scope struct {
	Scope      string
	Permission Permission
}

type Permission int

const (
	PermissionRead = 1 << iota
	PermissionWrite
)

func (p Permission) String() string {
	out := make([]string, 0, 2)
	if p&PermissionRead != 0 {
		out = append(out, "Read")
	}
	if p&PermissionWrite != 0 {
		out = append(out, "Write")
	}
	if p > PermissionRead|PermissionWrite {
		return strconv.Itoa(int(p))
	}
	return strings.Join(out, "|")
}

type Cache struct {
	scopes []Scope
	URL    string
	Token  *jwt.Token
}

func (c *Cache) Scopes() []Scope {
	return c.scopes
}

func (c *Cache) Load(ctx context.Context, keys ...string) (*Entry, error) {
	req, err := http.NewRequest("GET", c.url("cache"), nil)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	c.auth(req)
	c.accept(req)
	q := req.URL.Query()
	q.Set("keys", strings.Join(keys, ","))
	q.Set("version", version(keys[0]))
	req.URL.RawQuery = q.Encode()
	req = req.WithContext(ctx)
	Log("load cache %s", req.URL.String())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	if err := checkResponse(resp); err != nil {
		return nil, err
	}
	var ce Entry
	dt, err := ioutil.ReadAll(io.LimitReader(resp.Body, 32*1024))
	if err != nil {
		return nil, errors.WithStack(err)
	}
	if len(dt) == 0 {
		return nil, nil
	}
	if err := json.Unmarshal(dt, &ce); err != nil {
		return nil, errors.WithStack(err)
	}
	if ce.Key == "" {
		return nil, nil
	}
	return &ce, nil
}

func (c *Cache) reserve(ctx context.Context, key string) (int, error) {
	dt, err := json.Marshal(ReserveCacheReq{Key: key, Version: version(key)})
	if err != nil {
		return 0, errors.WithStack(err)
	}
	req, err := http.NewRequest("POST", c.url("caches"), bytes.NewReader(dt))
	if err != nil {
		return 0, errors.WithStack(err)
	}
	c.auth(req)
	c.accept(req)
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(ctx)
	Log("save cache req %s body=%s", req.URL.String(), dt)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, errors.WithStack(err)
	}
	if err := checkResponse(resp); err != nil {
		return 0, err
	}

	dt, err = ioutil.ReadAll(io.LimitReader(resp.Body, 32*1024))
	if err != nil {
		return 0, errors.WithStack(err)
	}
	var cr ReserveCacheResp
	if err := json.Unmarshal(dt, &cr); err != nil {
		return 0, errors.Wrapf(err, "failed to unmarshal %s", dt)
	}
	if cr.CacheID == 0 {
		return 0, errors.Errorf("invalid response %s", dt)
	}
	Log("save cache resp: %s", dt)
	return cr.CacheID, nil
}

func (c *Cache) commit(ctx context.Context, id int, size int64) error {
	dt, err := json.Marshal(CommitCacheReq{Size: size})
	if err != nil {
		return errors.WithStack(err)
	}
	req, err := http.NewRequest("POST", c.url(fmt.Sprintf("caches/%d", id)), bytes.NewReader(dt))
	if err != nil {
		return errors.WithStack(err)
	}
	c.auth(req)
	c.accept(req)
	req.Header.Set("Content-Type", "application/json")
	Log("commit cache %s, size %d", req.URL.String(), size)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return errors.Wrapf(err, "error committing cache %d", id)
	}
	if err := checkResponse(resp); err != nil {
		return err
	}
	dt, err = ioutil.ReadAll(io.LimitReader(resp.Body, 32*1024))
	if err != nil {
		return err
	}
	if len(dt) != 0 {
		Log("commit response: %s", dt)
	}
	return resp.Body.Close()
}

func (c *Cache) upload(ctx context.Context, id int, ra io.ReaderAt, size int64) error {
	var mu sync.Mutex
	eg, ctx := errgroup.WithContext(ctx)
	offset := int64(0)
	for i := 0; i < UploadConcurrency; i++ {
		eg.Go(func() error {
			for {
				mu.Lock()
				start := offset
				if start >= size {
					mu.Unlock()
					return nil
				}
				end := start + int64(UploadChunkSize)
				if end > size {
					end = size
				}
				offset = end
				mu.Unlock()

				if err := c.uploadChunk(ctx, id, ra, start, end-start); err != nil {
					return err
				}
			}
		})
	}
	return eg.Wait()
}

func (c *Cache) Save(ctx context.Context, key string, ra io.ReaderAt, size int64) error {
	id, err := c.reserve(ctx, key)
	if err != nil {
		return err
	}

	if err := c.upload(ctx, id, ra, size); err != nil {
		return err
	}

	return c.commit(ctx, id, size)
}

func (c *Cache) uploadChunk(ctx context.Context, id int, ra io.ReaderAt, off, n int64) error {
	r := io.NewSectionReader(ra, off, n)
	req, err := http.NewRequest("PATCH", c.url(fmt.Sprintf("caches/%d", id)), r)
	if err != nil {
		return errors.WithStack(err)
	}
	c.auth(req)
	c.accept(req)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/*", off, off+n-1))

	Log("upload cache chunk %s, range %d-%d", req.URL.String(), off, off+n-1)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return errors.WithStack(err)
	}
	if err := checkResponse(resp); err != nil {
		return err
	}
	dt, err := ioutil.ReadAll(io.LimitReader(resp.Body, 32*1024))
	if err != nil {
		return errors.WithStack(err)
	}
	if len(dt) != 0 {
		Log("upload chunk resp: %s", dt)
	}
	return resp.Body.Close()
}

func (c *Cache) auth(r *http.Request) {
	r.Header.Add("Authorization", "Bearer "+c.Token.Raw)
}

func (c *Cache) accept(r *http.Request) {
	r.Header.Add("Accept", "application/json;api-version=6.0-preview.1")
}

func (c *Cache) url(p string) string {
	return c.URL + "_apis/artifactcache/" + p
}

type ReserveCacheReq struct {
	Key     string `json:"key"`
	Version string `json:"version"`
}

type ReserveCacheResp struct {
	CacheID int `json:"cacheID"`
}

type CommitCacheReq struct {
	Size int64 `json:"size"`
}

type Entry struct {
	Key   string `json:"cacheKey"`
	Scope string `json:"scope"`
	URL   string `json:"archiveLocation"`
}

func (ce *Entry) Download(ctx context.Context, w io.Writer) error {
	req, err := http.NewRequest("GET", ce.URL, nil)
	if err != nil {
		return errors.WithStack(err)
	}
	req = req.WithContext(ctx)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return errors.WithStack(err)
	}
	_, err = io.Copy(w, resp.Body)
	return errors.WithStack(err)
}

func version(k string) string {
	h := sha256.New()
	// h.Write([]byte(k))
	// upstream uses paths in version, we don't seem to have anything that is unique like this
	h.Write([]byte("|go-actionscache-1.0"))
	return hex.EncodeToString(h.Sum(nil))
}

type GithubAPIError struct {
	Message   string `json:"message"`
	TypeName  string `json:"typeName"`
	TypeKey   string `json:"typeKey"`
	ErrorCode int    `json:"errorCode"`
}

func (e GithubAPIError) Error() string {
	return e.Message
}

func checkResponse(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	dt, err := ioutil.ReadAll(io.LimitReader(resp.Body, 32*1024))
	if err != nil {
		return errors.WithStack(err)
	}
	var gae GithubAPIError
	if err := json.Unmarshal(dt, &gae); err != nil {
		return errors.Wrapf(err, "failed to parse error response %d: %s", resp.StatusCode, dt)
	}
	if gae.Message != "" {
		return errors.WithStack(gae)
	}
	return errors.Errorf("unknown error %d: %s", resp.StatusCode, dt)
}

func decryptToken(enc, pass string) (string, string, error) {
	// openssl key derivation uses some non-standard algorithm so exec instead of using go libraries
	// this is only used on testing anyway
	cmd := exec.Command("openssl", "enc", "-d", "-aes-256-cbc", "-a", "-A", "-salt", "-md", "sha256", "-pass", "env:GOCACHE_TOKEN_PW")
	cmd.Env = append(cmd.Env, fmt.Sprintf("GOCACHE_TOKEN_PW=%s", pass))
	cmd.Stdin = bytes.NewReader([]byte(enc))
	buf := &bytes.Buffer{}
	cmd.Stdout = buf
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", "", err
	}
	parts := bytes.SplitN(buf.Bytes(), []byte(":::"), 2)
	if len(parts) != 2 {
		return "", "", errors.Errorf("invalid decrypt contents %s", buf.String())
	}
	return string(parts[0]), strings.TrimSpace(string(parts[1])), nil
}
