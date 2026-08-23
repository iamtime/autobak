package backend

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestLocalRoundtrip(t *testing.T) {
	ctx := context.Background()
	be, err := OpenLocal(t.TempDir(), Caps{CanWrite: true, CanDelete: true})
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("содержимое пака")
	if err := be.Put(ctx, "data/ab/abcdef", bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatal(err)
	}

	got, err := ReadAll(ctx, be, "data/ab/abcdef", 1<<20)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("чтение вернуло %q, %v", got, err)
	}

	rc, err := be.GetRange(ctx, "data/ab/abcdef", 11, 4)
	if err != nil {
		t.Fatal(err)
	}
	part, _ := io.ReadAll(rc)
	rc.Close()
	if string(part) != string(data[11:15]) {
		t.Fatalf("range вернул %q вместо %q", part, data[11:15])
	}

	st, err := be.Stat(ctx, "data/ab/abcdef")
	if err != nil || st.Size != int64(len(data)) {
		t.Fatalf("stat: %+v, %v", st, err)
	}

	var names []string
	if err := be.List(ctx, "data/", func(fi FileInfo) error {
		names = append(names, fi.Name)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "data/ab/abcdef" {
		t.Fatalf("листинг вернул %v", names)
	}

	if err := be.Delete(ctx, "data/ab/abcdef"); err != nil {
		t.Fatal(err)
	}
	if _, err := be.Get(ctx, "data/ab/abcdef"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("после удаления ожидалась ErrNotFound, получено %v", err)
	}
}

// Имена объектов приходят в том числе из метаданных репозитория.
// Выход за пределы каталога обязан отсекаться.
func TestLocalRejectsTraversal(t *testing.T) {
	ctx := context.Background()
	be, err := OpenLocal(t.TempDir(), Caps{CanWrite: true, CanDelete: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		"../secrets", "data/../../etc/passwd", "/etc/passwd", "", "a//b", `data\ab`,
	} {
		if err := be.Put(ctx, bad, strings.NewReader("x"), 1); err == nil {
			t.Errorf("имя %q было принято", bad)
		}
	}
}

// Отсутствие права удаления - не рекомендация, а запрет: на этом стоит
// защита бэкапов от захваченного сервера.
func TestCapsEnforced(t *testing.T) {
	ctx := context.Background()
	be, err := OpenLocal(t.TempDir(), Caps{CanWrite: true, CanDelete: false})
	if err != nil {
		t.Fatal(err)
	}
	if err := be.Put(ctx, "x", strings.NewReader("y"), 1); err != nil {
		t.Fatal(err)
	}
	if err := be.Delete(ctx, "x"); !errors.Is(err, ErrNoDelete) {
		t.Fatalf("удаление без права вернуло %v", err)
	}

	ro, err := OpenLocal(t.TempDir(), Caps{})
	if err == nil {
		if err := ro.Put(ctx, "x", strings.NewReader("y"), 1); !errors.Is(err, ErrReadOnly) {
			t.Fatalf("запись в режиме чтения вернула %v", err)
		}
	}
}

// Несовпадение заявленного и реального размера означает обрыв: молча
// записать короткий объект нельзя, иначе он потом «прочитается» как пустой.
func TestLocalDetectsShortWrite(t *testing.T) {
	ctx := context.Background()
	be, _ := OpenLocal(t.TempDir(), Caps{CanWrite: true})
	if err := be.Put(ctx, "x", strings.NewReader("abc"), 100); err == nil {
		t.Fatal("объект короче заявленного был принят")
	}
	if _, err := be.Stat(ctx, "x"); !errors.Is(err, ErrNotFound) {
		t.Fatal("после неудачной записи остался частичный объект")
	}
}

// --- S3 -------------------------------------------------------------------

type fakeS3 struct {
	mu        sync.Mutex
	objects   map[string][]byte
	lastAuth  string
	lastRange string
	fail      int // сколько первых запросов провалить с 503
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.lastAuth = r.Header.Get("Authorization")
	f.lastRange = r.Header.Get("Range")
	if f.fail > 0 {
		f.fail--
		w.WriteHeader(503)
		return
	}
	// Подпись обязана присутствовать и содержать все три подписанных
	// заголовка - без этого настоящий S3 ответит 403.
	if !strings.HasPrefix(f.lastAuth, "AWS4-HMAC-SHA256 ") ||
		!strings.Contains(f.lastAuth, "SignedHeaders=host;x-amz-content-sha256;x-amz-date") {
		w.WriteHeader(403)
		w.Write([]byte(`<Error><Code>SignatureDoesNotMatch</Code><Message>bad</Message></Error>`))
		return
	}

	key := strings.TrimPrefix(r.URL.Path, "/bucket/")
	switch r.Method {
	case http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		f.objects[key] = body
		w.WriteHeader(200)

	case http.MethodHead:
		b, ok := f.objects[key]
		if !ok {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(b)))
		w.WriteHeader(200)

	case http.MethodGet:
		if r.URL.Query().Get("list-type") == "2" {
			var sb strings.Builder
			sb.WriteString(`<?xml version="1.0"?><ListBucketResult>`)
			prefix := r.URL.Query().Get("prefix")
			for k, v := range f.objects {
				if strings.HasPrefix(k, prefix) {
					fmt.Fprintf(&sb, `<Contents><Key>%s</Key><Size>%d</Size>`+
						`<LastModified>2026-08-21T00:00:00.000Z</LastModified></Contents>`, k, len(v))
				}
			}
			sb.WriteString(`<IsTruncated>false</IsTruncated></ListBucketResult>`)
			w.Write([]byte(sb.String()))
			return
		}
		b, ok := f.objects[key]
		if !ok {
			w.WriteHeader(404)
			return
		}
		if rg := r.Header.Get("Range"); rg != "" {
			var from, to int
			fmt.Sscanf(rg, "bytes=%d-%d", &from, &to)
			if to >= len(b) {
				to = len(b) - 1
			}
			w.WriteHeader(206)
			w.Write(b[from : to+1])
			return
		}
		w.Write(b)

	case http.MethodDelete:
		delete(f.objects, key)
		w.WriteHeader(204)
	}
}

func newFakeS3(t *testing.T) (*S3, *fakeS3) {
	t.Helper()
	fake := &fakeS3{objects: map[string][]byte{}}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	be, err := OpenS3(S3Config{
		Endpoint: srv.URL, Region: "ru-1", Bucket: "bucket", Prefix: "repo",
		AccessKey: "AKIATEST", SecretKey: "секретный-ключ", PathStyle: true,
		Caps: Caps{CanWrite: true, CanDelete: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	return be, fake
}

func TestS3Roundtrip(t *testing.T) {
	ctx := context.Background()
	be, fake := newFakeS3(t)

	data := bytes.Repeat([]byte("пак"), 1000)
	if err := be.Put(ctx, "data/ab/pack1", bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatal(err)
	}
	if _, ok := fake.objects["repo/data/ab/pack1"]; !ok {
		t.Fatalf("объект лёг не туда: %v", keysOf(fake.objects))
	}

	got, err := ReadAll(ctx, be, "data/ab/pack1", 1<<20)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("чтение: %d байт, %v", len(got), err)
	}

	rc, err := be.GetRange(ctx, "data/ab/pack1", 6, 3)
	if err != nil {
		t.Fatal(err)
	}
	part, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(part, data[6:9]) {
		t.Fatalf("range вернул %q вместо %q", part, data[6:9])
	}
	if fake.lastRange != "bytes=6-8" {
		t.Fatalf("отправлен заголовок Range %q", fake.lastRange)
	}

	var names []string
	if err := be.List(ctx, "data/", func(fi FileInfo) error {
		names = append(names, fi.Name)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Префикс бакета из имён обязан быть срезан: остальной код работает
	// с именами вида data/ab/pack1 и про префикс ничего не знает.
	if len(names) != 1 || names[0] != "data/ab/pack1" {
		t.Fatalf("листинг вернул %v", names)
	}

	if err := be.Delete(ctx, "data/ab/pack1"); err != nil {
		t.Fatal(err)
	}
	if _, err := be.Stat(ctx, "data/ab/pack1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("после удаления: %v", err)
	}
}

// Временные 5xx - обычное дело у любого хранилища. Ронять из-за них
// многочасовой бэкап нельзя.
func TestS3RetriesServerErrors(t *testing.T) {
	ctx := context.Background()
	be, fake := newFakeS3(t)
	fake.fail = 2

	payload := "данные"
	if err := be.Put(ctx, "x", strings.NewReader(payload), int64(len(payload))); err != nil {
		t.Fatalf("повторы не спасли от временной ошибки: %v", err)
	}
	if string(fake.objects["repo/x"]) != "данные" {
		t.Fatal("объект не записался после повторов")
	}
}

func TestS3RequiresHTTPS(t *testing.T) {
	_, err := OpenS3(S3Config{
		Endpoint: "http://s3.example.com", Bucket: "b",
		AccessKey: "a", SecretKey: "s",
	})
	if err == nil {
		t.Fatal("незашифрованное соединение с хранилищем было принято")
	}
}

func TestS3RespectsCaps(t *testing.T) {
	ctx := context.Background()
	fake := &fakeS3{objects: map[string][]byte{}}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	// Так настраивается агент на сервере: писать может, удалять - нет.
	be, err := OpenS3(S3Config{
		Endpoint: srv.URL, Bucket: "bucket", AccessKey: "a", SecretKey: "s",
		PathStyle: true, Caps: Caps{CanWrite: true, CanDelete: false},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := be.Put(ctx, "x", strings.NewReader("y"), 1); err != nil {
		t.Fatal(err)
	}
	if err := be.Delete(ctx, "x"); !errors.Is(err, ErrNoDelete) {
		t.Fatalf("удаление прошло мимо ограничения: %v", err)
	}
}

func TestURIEncode(t *testing.T) {
	cases := map[string]string{
		"abc":   "abc",
		"a b":   "a%20b",
		"a/b":   "a%2Fb",
		"a+b":   "a%2Bb",
		"тест":  "%D1%82%D0%B5%D1%81%D1%82",
		"a-_.~": "a-_.~",
	}
	for in, want := range cases {
		if got := uriEncode(in); got != want {
			t.Errorf("uriEncode(%q) = %q, ожидалось %q", in, got, want)
		}
	}
	if got := uriEncodePath("/bucket/a b/c"); got != "/bucket/a%20b/c" {
		t.Errorf("uriEncodePath дал %q", got)
	}
}

func keysOf(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// PutNew не должен перезаписывать существующий объект: это защита
// репозитория от затирания паков/индексов/ключей мусором.
func TestPutNewRefusesOverwrite(t *testing.T) {
	ctx := context.Background()
	be, err := OpenLocal(t.TempDir(), Caps{CanWrite: true, CanDelete: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := be.PutNew(ctx, "data/aa/pack1", strings.NewReader("original"), 8); err != nil {
		t.Fatalf("первая запись PutNew: %v", err)
	}
	// Повторная запись тем же именем - ErrExists, содержимое не тронуто.
	err = be.PutNew(ctx, "data/aa/pack1", strings.NewReader("tampered"), 8)
	if !errors.Is(err, ErrExists) {
		t.Fatalf("PutNew поверх существующего вернул %v, ожидался ErrExists", err)
	}
	rc, err := be.Get(ctx, "data/aa/pack1")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != "original" {
		t.Fatalf("объект перезаписан: %q", got)
	}
}
