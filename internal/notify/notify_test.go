package notify

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestShouldSendDefaults(t *testing.T) {
	c := DefaultConfig()
	c.Enabled = true

	if !c.ShouldSend(Message{Level: LevelError}) {
		t.Error("об ошибке обязаны сообщать всегда")
	}
	if !c.ShouldSend(Message{Level: LevelWarning}) {
		t.Error("о неполном снимке обязаны сообщать: он выглядит целым, но им не является")
	}
	// Ежедневное «всё хорошо» перестают читать, и вместе с ним
	// перестают замечать единственное «всё плохо».
	if c.ShouldSend(Message{Level: LevelInfo}) {
		t.Error("об успехе по умолчанию сообщать не должны")
	}

	off := DefaultConfig()
	if off.ShouldSend(Message{Level: LevelError}) {
		t.Error("выключенные уведомления не должны отправляться")
	}
}

func TestTelegramSendsPlainText(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "chat_id") {
			t.Errorf("не тот запрос: %s", body)
		}
		got = map[string]any{"body": string(body), "path": r.URL.Path}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	// Подменяем адрес API через транспорт: сам код собирает его из токена.
	old := client
	client = srv.Client()
	client.Transport = rewriteTo(srv.URL)
	defer func() { client = old }()

	cfg := DefaultConfig()
	cfg.Enabled = true
	cfg.Telegram = TelegramConfig{ChatID: "42", Token: "секретный-токен"}

	err := Send(context.Background(), cfg, Message{
		Level: LevelError, Server: "prod", Title: "бэкап не прошёл",
		Body: "база admin_shop не выгружена",
	})
	if err != nil {
		t.Fatal(err)
	}
	body := got["body"].(string)
	for _, must := range []string{"prod", "бэкап не прошёл", "admin_shop", "СБОЙ"} {
		if !strings.Contains(body, must) {
			t.Errorf("в сообщении нет %q: %s", must, body)
		}
	}
}

// Токен бота - это полный доступ к боту, и он вписан прямо в адрес API.
// При сетевом сбое Go подставляет адрес в текст ошибки целиком, а ошибки
// уходят в журнал и на экран. Значит, вычищать токен обязательно.
func TestTelegramErrorHidesToken(t *testing.T) {
	old := client
	client = &http.Client{Transport: failingTransport{}}
	defer func() { client = old }()

	cfg := DefaultConfig()
	cfg.Enabled = true
	cfg.Telegram = TelegramConfig{ChatID: "42", Token: "СУПЕРСЕКРЕТ"}

	err := Send(context.Background(), cfg, Message{Level: LevelError, Title: "x"})
	if err == nil {
		t.Fatal("ошибка доставки не замечена")
	}
	if strings.Contains(err.Error(), "СУПЕРСЕКРЕТ") {
		t.Fatalf("токен утёк в текст ошибки: %v", err)
	}
	if !strings.Contains(err.Error(), "***") {
		t.Fatalf("токен не заменён на маску: %v", err)
	}
	t.Logf("ошибка без секрета: %v", err)
}

type failingTransport struct{}

func (failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("сеть недоступна")
}

func TestWebhookAndHeartbeat(t *testing.T) {
	var hookBody string
	var pinged bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ping" {
			pinged = true
			return
		}
		b, _ := io.ReadAll(r.Body)
		hookBody = string(b)
	}))
	defer srv.Close()

	cfg := DefaultConfig()
	cfg.Enabled = true
	cfg.Webhook = WebhookConfig{URL: srv.URL + "/hook"}

	if err := Send(context.Background(), cfg, Message{
		Level: LevelWarning, Server: "prod", Title: "снимок неполон",
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(hookBody, "снимок неполон") || !strings.Contains(hookBody, "ВНИМАНИЕ") {
		t.Fatalf("webhook получил: %s", hookBody)
	}

	if err := Heartbeat(context.Background(), srv.URL+"/ping"); err != nil {
		t.Fatal(err)
	}
	if !pinged {
		t.Fatal("сигнал «я жив» не дошёл")
	}
}

// Тема письма с кириллицей обязана кодироваться, иначе почтовые клиенты
// показывают кракозябры.
func TestMimeEncode(t *testing.T) {
	got := encodeHeader("[СБОЙ] prod: бэкап не прошёл")
	if !strings.HasPrefix(got, "=?UTF-8?B?") || !strings.HasSuffix(got, "?=") {
		t.Fatalf("тема не закодирована: %s", got)
	}
	payload := strings.TrimSuffix(strings.TrimPrefix(got, "=?UTF-8?B?"), "?=")
	dec, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		t.Fatalf("некорректный base64: %v", err)
	}
	if string(dec) != "[СБОЙ] prod: бэкап не прошёл" {
		t.Fatalf("после раскодирования: %q", dec)
	}
	// ASCII кодировать незачем.
	if encodeHeader("simple subject") != "simple subject" {
		t.Error("ASCII-тема закодирована без нужды")
	}
}

func TestStaleMessage(t *testing.T) {
	m := Stale("prod", time.Now().Add(-50*time.Hour), 36*time.Hour)
	if m.Level != LevelError {
		t.Error("залежавшийся бэкап - это ошибка, а не предупреждение")
	}
	if !strings.Contains(m.Title, "2 дн") {
		t.Errorf("срок не назван по-человечески: %s", m.Title)
	}

	never := Stale("prod", time.Time{}, time.Hour)
	if !strings.Contains(never.Title, "ни разу") {
		t.Errorf("случай «не было ни разу» не выделен: %s", never.Title)
	}
}

// rewriteTo перенаправляет любые запросы на тестовый сервер, сохраняя путь.
type rewriter struct{ base string }

func rewriteTo(base string) http.RoundTripper { return &rewriter{base: base} }

func (rw *rewriter) RoundTrip(r *http.Request) (*http.Response, error) {
	u := *r.URL
	target := strings.TrimPrefix(rw.base, "http://")
	u.Host = target
	u.Scheme = "http"
	r2 := r.Clone(r.Context())
	r2.URL = &u
	r2.Host = target
	return http.DefaultTransport.RoundTrip(r2)
}
