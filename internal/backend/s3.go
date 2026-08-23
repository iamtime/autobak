package backend

import (
	"bytes"
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
	"sort"
	"strconv"
	"strings"
	"time"
)

// S3 - минимальный клиент S3-совместимого хранилища (AWS, Backblaze B2,
// Wasabi, MinIO, Selectel, Timeweb).
//
// Написан вручную вместо aws-sdk-go: SDK добавляет около 15 МБ к бинарю
// агента, а нужно всего шесть операций. Здесь ровно они и SigV4.
type S3 struct {
	cli       *http.Client
	endpoint  *url.URL
	region    string
	bucket    string
	prefix    string
	accessKey string
	secretKey string
	pathStyle bool
	caps      Caps
}

type S3Config struct {
	Endpoint  string // https://s3.us-west-004.backblazeb2.com
	Region    string // us-west-004
	Bucket    string
	Prefix    string // необязательный подкаталог внутри бакета
	AccessKey string
	SecretKey string
	// PathStyle: https://endpoint/bucket/key вместо https://bucket.endpoint/key.
	// MinIO и большинство российских провайдеров понимают только его.
	PathStyle bool
	Caps      Caps
}

// maxPutSize - потолок для одного объекта. Всё, что autobak кладёт в S3
// (паки, индексы, снимки), заведомо меньше; ограничение существует, чтобы
// ошибка в вызывающем коде не превратилась в попытку выделить гигабайты.
const maxPutSize = 256 * 1024 * 1024

func OpenS3(cfg S3Config) (*S3, error) {
	if cfg.Bucket == "" {
		return nil, errors.New("autobak: не указан бакет")
	}
	if cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, errors.New("autobak: не указаны ключи доступа к хранилищу")
	}
	ep := cfg.Endpoint
	if !strings.Contains(ep, "://") {
		ep = "https://" + ep
	}
	u, err := url.Parse(ep)
	if err != nil {
		return nil, fmt.Errorf("autobak: некорректный endpoint: %w", err)
	}
	if u.Scheme != "https" && !strings.HasPrefix(u.Host, "127.0.0.1") && !strings.HasPrefix(u.Host, "localhost") {
		return nil, errors.New("autobak: хранилище доступно только по https (исключение - localhost для тестов)")
	}
	region := cfg.Region
	if region == "" {
		region = "us-east-1"
	}
	return &S3{
		cli: &http.Client{Timeout: 10 * time.Minute},

		endpoint:  u,
		region:    region,
		bucket:    cfg.Bucket,
		prefix:    strings.Trim(cfg.Prefix, "/"),
		accessKey: cfg.AccessKey,
		secretKey: cfg.SecretKey,
		pathStyle: cfg.PathStyle,
		caps:      cfg.Caps,
	}, nil
}

func (s *S3) Location() string {
	loc := s.endpoint.Host + "/" + s.bucket
	if s.prefix != "" {
		loc += "/" + s.prefix
	}
	return "s3://" + loc
}

func (s *S3) Caps() Caps   { return s.caps }
func (s *S3) Close() error { s.cli.CloseIdleConnections(); return nil }

func (s *S3) key(name string) string {
	if s.prefix == "" {
		return name
	}
	return s.prefix + "/" + name
}

// url собирает адрес объекта и одновременно возвращает канонический путь
// для подписи - они обязаны совпадать байт в байт, иначе будет 403.
func (s *S3) url(key string, query url.Values) (string, string) {
	var host, path string
	if s.pathStyle {
		host = s.endpoint.Host
		path = "/" + s.bucket
	} else {
		host = s.bucket + "." + s.endpoint.Host
		path = ""
	}
	if key != "" {
		path += "/" + key
	}
	if path == "" {
		path = "/"
	}
	canonPath := uriEncodePath(path)
	full := s.endpoint.Scheme + "://" + host + canonPath
	if len(query) > 0 {
		full += "?" + canonicalQuery(query)
	}
	return full, canonPath
}

func (s *S3) do(ctx context.Context, method, key string, query url.Values, body []byte, hdr http.Header) (*http.Response, error) {
	var lastErr error
	for attempt := range 4 {
		if attempt > 0 {
			// Пауза перед повтором: сеть и хранилища регулярно отдают
			// временные 500/503, ронять из-за них весь бэкап нельзя.
			select {
			case <-time.After(time.Duration(1<<attempt) * time.Second):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		resp, err := s.attempt(ctx, method, key, query, body, hdr)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode >= 500 || resp.StatusCode == 429 {
			msg := s3Error(resp)
			resp.Body.Close()
			lastErr = fmt.Errorf("autobak: хранилище ответило %d: %s", resp.StatusCode, msg)
			continue
		}
		return resp, nil
	}
	return nil, lastErr
}

func (s *S3) attempt(ctx context.Context, method, key string, query url.Values, body []byte, hdr http.Header) (*http.Response, error) {
	full, canonPath := s.url(key, query)
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, full, rdr)
	if err != nil {
		return nil, err
	}
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if body != nil {
		req.ContentLength = int64(len(body))
	}
	s.sign(req, canonPath, query, body)
	return s.cli.Do(req)
}

func (s *S3) sign(req *http.Request, canonPath string, query url.Values, body []byte) {
	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")

	sum := sha256.Sum256(body) // nil body даёт хэш пустой строки - это верно
	payloadHash := hex.EncodeToString(sum[:])

	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	req.Header.Set("Host", req.URL.Host)

	// Подписываем host, дату и хэш тела. Range и Content-Type умышленно
	// не подписываем: часть прокси их переписывает, и подпись ломается.
	signed := []string{"host", "x-amz-content-sha256", "x-amz-date"}
	var canonHeaders strings.Builder
	for _, h := range signed {
		v := req.Header.Get(h)
		if h == "host" {
			v = req.URL.Host
		}
		canonHeaders.WriteString(h + ":" + strings.TrimSpace(v) + "\n")
	}
	signedHeaders := strings.Join(signed, ";")

	canonReq := strings.Join([]string{
		req.Method, canonPath, canonicalQuery(query),
		canonHeaders.String(), signedHeaders, payloadHash,
	}, "\n")

	scope := strings.Join([]string{dateStamp, s.region, "s3", "aws4_request"}, "/")
	crSum := sha256.Sum256([]byte(canonReq))
	toSign := strings.Join([]string{
		"AWS4-HMAC-SHA256", amzDate, scope, hex.EncodeToString(crSum[:]),
	}, "\n")

	k := hmacSHA256([]byte("AWS4"+s.secretKey), dateStamp)
	k = hmacSHA256(k, s.region)
	k = hmacSHA256(k, "s3")
	k = hmacSHA256(k, "aws4_request")
	sig := hex.EncodeToString(hmacSHA256(k, toSign))

	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		s.accessKey, scope, signedHeaders, sig))
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

func canonicalQuery(q url.Values) string {
	if len(q) == 0 {
		return ""
	}
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
			parts = append(parts, uriEncode(k)+"="+uriEncode(v))
		}
	}
	return strings.Join(parts, "&")
}

// uriEncode кодирует по правилам AWS: не трогает unreserved-символы,
// остальное переводит в %XX заглавными буквами. url.QueryEscape здесь
// не подходит - он кодирует пробел как "+", а подпись этого не примет.
func uriEncode(s string) string {
	var b strings.Builder
	for i := range len(s) {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func uriEncodePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = uriEncode(s)
	}
	return strings.Join(segs, "/")
}

func s3Error(resp *http.Response) string {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8*1024))
	var e struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}
	if xml.Unmarshal(body, &e) == nil && e.Code != "" {
		return e.Code + ": " + e.Message
	}
	if len(body) == 0 {
		return resp.Status
	}
	return strings.TrimSpace(string(body))
}

func (s *S3) statusErr(resp *http.Response) error {
	if resp.StatusCode == 404 {
		resp.Body.Close()
		return ErrNotFound
	}
	if resp.StatusCode == 403 {
		msg := s3Error(resp)
		resp.Body.Close()
		return fmt.Errorf("autobak: хранилище отклонило запрос (403): %s. Проверьте ключи, регион и права", msg)
	}
	if resp.StatusCode >= 400 {
		msg := s3Error(resp)
		resp.Body.Close()
		return fmt.Errorf("autobak: хранилище ответило %d: %s", resp.StatusCode, msg)
	}
	return nil
}

func (s *S3) Get(ctx context.Context, name string) (io.ReadCloser, error) {
	resp, err := s.do(ctx, http.MethodGet, s.key(name), nil, nil, nil)
	if err != nil {
		return nil, err
	}
	if err := s.statusErr(resp); err != nil {
		return nil, err
	}
	return resp.Body, nil
}

func (s *S3) GetRange(ctx context.Context, name string, off, length int64) (io.ReadCloser, error) {
	hdr := http.Header{}
	hdr.Set("Range", fmt.Sprintf("bytes=%d-%d", off, off+length-1))
	resp, err := s.do(ctx, http.MethodGet, s.key(name), nil, nil, hdr)
	if err != nil {
		return nil, err
	}
	if err := s.statusErr(resp); err != nil {
		return nil, err
	}
	// 200 вместо 206 означает, что Range проигнорирован и пришёл весь объект.
	// Молча отдать не тот кусок нельзя - обрезаем сами.
	if resp.StatusCode == http.StatusOK && off > 0 {
		if _, err := io.CopyN(io.Discard, resp.Body, off); err != nil {
			resp.Body.Close()
			return nil, err
		}
	}
	return struct {
		io.Reader
		io.Closer
	}{io.LimitReader(resp.Body, length), resp.Body}, nil
}

func (s *S3) Put(ctx context.Context, name string, r io.Reader, size int64) error {
	return s.put(ctx, name, r, size, false)
}

// PutNew пишет объект, только если его ещё нет (заголовок If-None-Match: *).
func (s *S3) PutNew(ctx context.Context, name string, r io.Reader, size int64) error {
	return s.put(ctx, name, r, size, true)
}

func (s *S3) put(ctx context.Context, name string, r io.Reader, size int64, createOnly bool) error {
	if !s.caps.CanWrite {
		return ErrReadOnly
	}
	if size > maxPutSize {
		return fmt.Errorf("autobak: объект %s размером %d байт слишком велик для одного запроса", name, size)
	}
	// Тело держим в памяти: SigV4 требует хэш содержимого до отправки,
	// а повтор при 503 требует возможности перечитать тело.
	body, err := io.ReadAll(io.LimitReader(r, maxPutSize+1))
	if err != nil {
		return err
	}
	if int64(len(body)) > maxPutSize {
		return fmt.Errorf("autobak: объект %s превысил допустимый размер", name)
	}
	if size >= 0 && int64(len(body)) != size {
		return fmt.Errorf("autobak: %s: прочитано %d байт вместо %d", name, len(body), size)
	}
	hdr := http.Header{}
	hdr.Set("Content-Type", "application/octet-stream")
	if createOnly {
		// If-None-Match: * - запись только при отсутствии объекта.
		// Поддерживается S3 и большинством совместимых; провайдер, который
		// его игнорирует, просто не даёт этой защиты, но не ломается.
		hdr.Set("If-None-Match", "*")
	}
	resp, err := s.do(ctx, http.MethodPut, s.key(name), nil, body, hdr)
	if err != nil {
		return err
	}
	if createOnly && resp.StatusCode == http.StatusPreconditionFailed {
		// Объект уже есть. Имя неизменяемого объекта привязано к
		// содержимому или уникально, поэтому это те же байты - повтор
		// после потерянного ответа, а не конфликт.
		resp.Body.Close()
		return ErrExists
	}
	if err := s.statusErr(resp); err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (s *S3) Stat(ctx context.Context, name string) (FileInfo, error) {
	resp, err := s.do(ctx, http.MethodHead, s.key(name), nil, nil, nil)
	if err != nil {
		return FileInfo{}, err
	}
	if err := s.statusErr(resp); err != nil {
		return FileInfo{}, err
	}
	defer resp.Body.Close()
	size, _ := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64)
	mt, _ := http.ParseTime(resp.Header.Get("Last-Modified"))
	return FileInfo{Name: name, Size: size, ModTime: mt}, nil
}

type listResult struct {
	IsTruncated bool   `xml:"IsTruncated"`
	NextToken   string `xml:"NextContinuationToken"`
	Contents    []struct {
		Key          string    `xml:"Key"`
		Size         int64     `xml:"Size"`
		LastModified time.Time `xml:"LastModified"`
	} `xml:"Contents"`
}

func (s *S3) List(ctx context.Context, prefix string, fn func(FileInfo) error) error {
	token := ""
	for {
		q := url.Values{}
		q.Set("list-type", "2")
		q.Set("prefix", s.key(prefix))
		q.Set("max-keys", "1000")
		if token != "" {
			q.Set("continuation-token", token)
		}
		resp, err := s.do(ctx, http.MethodGet, "", q, nil, nil)
		if err != nil {
			return err
		}
		if err := s.statusErr(resp); err != nil {
			return err
		}
		var res listResult
		err = xml.NewDecoder(io.LimitReader(resp.Body, 8*1024*1024)).Decode(&res)
		resp.Body.Close()
		if err != nil {
			return fmt.Errorf("autobak: не разобрать ответ хранилища: %w", err)
		}
		for _, c := range res.Contents {
			name := strings.TrimPrefix(c.Key, s.key(""))
			name = strings.TrimPrefix(name, "/")
			if err := fn(FileInfo{Name: name, Size: c.Size, ModTime: c.LastModified}); err != nil {
				return err
			}
		}
		if !res.IsTruncated {
			return nil
		}
		if res.NextToken == "" {
			// Хранилище говорит «список неполон», но не даёт токена для
			// продолжения. Молча вернуть усечённый список нельзя: prune
			// счёл бы отсутствующие снимки удалёнными и стёр бы их чанки,
			// mirror скопировал бы не всё. Лучше громкая ошибка.
			return fmt.Errorf("autobak: хранилище вернуло усечённый список без токена продолжения "+
				"(prefix %q) - оно не поддерживает постраничный листинг V2", prefix)
		}
		token = res.NextToken
	}
}

func (s *S3) Delete(ctx context.Context, name string) error {
	if !s.caps.CanDelete {
		return ErrNoDelete
	}
	resp, err := s.do(ctx, http.MethodDelete, s.key(name), nil, nil, nil)
	if err != nil {
		return err
	}
	if err := s.statusErr(resp); err != nil && !errors.Is(err, ErrNotFound) {
		return err
	} else if err == nil {
		resp.Body.Close()
	}
	return nil
}
