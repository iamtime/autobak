package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/iamtime/autobak/internal/app"
	"github.com/iamtime/autobak/internal/uiapi"
)

func testServer(t *testing.T) (*server, http.Handler) {
	t.Helper()
	a, err := app.OpenAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	au, err := newAuth("admin", "правильный-пароль")
	if err != nil {
		t.Fatal(err)
	}
	s := newServer(uiapi.New(a, nil), au, serverOptions{})
	return s, s.routes()
}

// Пока не вошёл - не видно ничего. Ни страницы, ни API: через этот
// интерфейс доступно восстановление на любой сервер.
func TestUnauthorizedSeesNothing(t *testing.T) {
	_, h := testServer(t)

	page := httptest.NewRecorder()
	h.ServeHTTP(page, httptest.NewRequest("GET", "/", nil))
	if page.Code != http.StatusSeeOther {
		t.Errorf("страница без входа: код %d, ожидалось перенаправление", page.Code)
	}

	api := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/State", strings.NewReader("[]"))
	req.Header.Set("X-Autobak", "1")
	h.ServeHTTP(api, req)
	if api.Code != http.StatusUnauthorized {
		t.Errorf("API без входа: код %d, ожидалось 401", api.Code)
	}
}

func login(t *testing.T, h http.Handler, user, pass string) *http.Cookie {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/login",
		strings.NewReader("user="+user+"&password="+pass))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.ServeHTTP(rec, req)
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			return c
		}
	}
	return nil
}

func TestLoginAndAPICall(t *testing.T) {
	_, h := testServer(t)

	if c := login(t, h, "admin", "неверный"); c != nil {
		t.Fatal("неверный пароль выдал сессию")
	}
	if c := login(t, h, "чужой", "правильный-пароль"); c != nil {
		t.Fatal("неверное имя выдало сессию")
	}

	c := login(t, h, "admin", "правильный-пароль")
	if c == nil {
		t.Fatal("верные данные не выдали сессию")
	}
	if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
		t.Errorf("кука сессии без защиты: HttpOnly=%v SameSite=%v", c.HttpOnly, c.SameSite)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/State", strings.NewReader("[]"))
	req.Header.Set("X-Autobak", "1")
	req.AddCookie(c)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("State: код %d, тело %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"version"`) {
		t.Errorf("ответ не похож на состояние: %s", rec.Body.String())
	}
}

// Заголовок обязателен: без него форма с чужого сайта могла бы вызвать
// удаление снимков за вошедшего человека.
func TestAPIRequiresHeader(t *testing.T) {
	_, h := testServer(t)
	c := login(t, h, "admin", "правильный-пароль")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/State", strings.NewReader("[]"))
	req.AddCookie(c)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("запрос без заголовка прошёл: код %d", rec.Code)
	}
}

func TestAPIDispatchErrors(t *testing.T) {
	_, h := testServer(t)
	c := login(t, h, "admin", "правильный-пароль")

	call := func(method, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/api/"+method, strings.NewReader(body))
		req.Header.Set("X-Autobak", "1")
		req.AddCookie(c)
		h.ServeHTTP(rec, req)
		return rec
	}

	cases := []struct {
		name, method, body string
		want               int
	}{
		{"несуществующий метод", "НетТакого", "[]", http.StatusNotFound},
		{"метод из чёрного списка", "App", "[]", http.StatusNotFound},
		{"ещё один из списка", "SetEmit", "[]", http.StatusNotFound},
		{"не столько аргументов", "CheckServer", "[]", http.StatusBadRequest},
		{"аргумент не того типа", "CheckServer", "[123]", http.StatusBadRequest},
		{"тело не список", "State", "{}", http.StatusBadRequest},
		{"ошибка внутри метода", "CheckServer", `["нет-такого"]`, http.StatusConflict},
	}
	for _, tc := range cases {
		if got := call(tc.method, tc.body).Code; got != tc.want {
			t.Errorf("%s: код %d, ожидался %d", tc.name, got, tc.want)
		}
	}
}

// App отдаёт ядро целиком, SetEmit принимает функцию. Ни то, ни другое
// не должно быть доступно снаружи, поэтому список закрытых методов
// проверяется отдельно от общего разбора ошибок.
func TestNotCallableCoversDangerousMethods(t *testing.T) {
	for _, want := range []string{"App", "SetEmit"} {
		found := false
		for _, have := range notCallable {
			if have == want {
				found = true
			}
		}
		if !found {
			t.Errorf("%s должен быть в списке недоступных методов", want)
		}
	}
}

func TestSessionExpires(t *testing.T) {
	au, err := newAuth("admin", "правильный-пароль")
	if err != nil {
		t.Fatal(err)
	}
	token, err := au.newSession()
	if err != nil {
		t.Fatal(err)
	}
	if !au.valid(token) {
		t.Fatal("свежая сессия недействительна")
	}

	au.mu.Lock()
	au.sessions[token] = time.Now().Add(-time.Minute)
	au.mu.Unlock()
	if au.valid(token) {
		t.Error("просроченная сессия принята")
	}

	// Выход должен закрывать сессию немедленно, а не ждать срока.
	token2, _ := au.newSession()
	au.drop(token2)
	if au.valid(token2) {
		t.Error("сессия жива после выхода")
	}
}

// Подбор пароля должен упираться в растущую паузу, а не идти вечно.
func TestFailedAttemptsBackOff(t *testing.T) {
	au, err := newAuth("admin", "правильный-пароль")
	if err != nil {
		t.Fatal(err)
	}
	const addr = "203.0.113.7:5555"

	for i := range 4 {
		au.failed(addr)
		if blocked, _ := au.blocked(addr); blocked {
			t.Fatalf("блокировка после %d попыток - человек ошибается и дважды, и трижды", i+1)
		}
	}
	au.failed(addr)
	blocked, wait := au.blocked(addr)
	if !blocked || wait <= 0 {
		t.Fatalf("после пяти неудач подбор не замедлен: blocked=%v wait=%v", blocked, wait)
	}

	// Удачный вход снимает наказание: иначе один опечатавшийся человек
	// заблокировал бы себе доступ к собственным бэкапам.
	au.succeeded(addr)
	if blocked, _ := au.blocked(addr); blocked {
		t.Error("удачный вход не снял блокировку")
	}
}

// Пароль не должен храниться в открытом виде: дамп памяти процесса и
// список окружения не выдают его.
func TestPasswordIsNotStored(t *testing.T) {
	au, err := newAuth("admin", "секретный-пароль-42")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(au.hash), "секретный") || strings.Contains(string(au.salt), "секретный") {
		t.Error("пароль виден в состоянии авторизации")
	}
	if !au.check("admin", "секретный-пароль-42") {
		t.Error("верный пароль не принят")
	}
	if au.check("admin", "секретный-пароль-4") {
		t.Error("принят пароль, отличающийся на символ")
	}
}

func TestShortPasswordRefused(t *testing.T) {
	if _, err := newAuth("admin", "1234567"); err == nil {
		t.Error("пароль из семи символов принят")
	}
}

// Заголовки безопасности ставятся на всё, включая страницу входа:
// именно на ней вводят пароль.
func TestSecurityHeaders(t *testing.T) {
	_, h := testServer(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/login", nil))

	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'self'") || !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("слабая политика содержимого: %q", csp)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("нет X-Content-Type-Options")
	}
}

// Рассылка событий идёт из горутины бэкапа, а подписки и отписки - из
// горутин запросов. Проверяем, что медленный подписчик теряет события,
// а не задерживает того, кто их шлёт: подвесить бэкап из-за забытой
// вкладки было бы куда хуже потерянной строчки в журнале.
func TestHubDoesNotBlockOnSlowClient(t *testing.T) {
	h := newHub()
	slow := h.add()
	defer h.remove(slow)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 10000 {
			h.broadcast("progress", map[string]int{"files": i})
		}
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("рассылка встала из-за подписчика, который не читает")
	}
}

func TestHubDeliversToEveryClient(t *testing.T) {
	h := newHub()
	a, b := h.add(), h.add()
	defer h.remove(a)
	defer h.remove(b)

	h.broadcast("log", LogPayload{Level: "warn", Msg: "проверка"})

	for name, ch := range map[string]chan sseEvent{"первый": a, "второй": b} {
		select {
		case ev := <-ch:
			if ev.Name != "log" || !strings.Contains(string(ev.Data), "проверка") {
				t.Errorf("%s подписчик получил %s %s", name, ev.Name, ev.Data)
			}
		case <-time.After(time.Second):
			t.Errorf("%s подписчик не получил событие", name)
		}
	}
}

// LogPayload повторяет форму строки журнала для проверки рассылки.
type LogPayload struct {
	Level string `json:"level"`
	Msg   string `json:"msg"`
}

// H6: за TLS-прокси (secure=true) кука сессии обязана быть Secure и
// сопровождаться HSTS, даже если сам сервер слушает по HTTP.
func TestSecureCookieBehindProxy(t *testing.T) {
	a, err := app.OpenAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	au, _ := newAuth("admin", "правильный-пароль")
	s := newServer(uiapi.New(a, nil), au, serverOptions{secure: true})
	h := s.routes()

	c := login(t, h, "admin", "правильный-пароль")
	if c == nil || !c.Secure {
		t.Fatalf("кука не помечена Secure за TLS-прокси: %+v", c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/login", nil))
	if rec.Header().Get("Strict-Transport-Security") == "" {
		t.Error("нет HSTS при secure-режиме")
	}
}

// L2: выход должен требовать служебный заголовок, иначе форма с чужого
// сайта разлогинивала бы администратора.
func TestLogoutRequiresHeader(t *testing.T) {
	_, h := testServer(t)
	c := login(t, h, "admin", "правильный-пароль")

	// Без заголовка - отказ.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/logout", nil)
	req.AddCookie(c)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("logout без заголовка прошёл: код %d", rec.Code)
	}
	// Сессия всё ещё жива.
	if !s0valid(h, c) {
		t.Fatal("сессия убита logout-ом без заголовка")
	}

	// С заголовком - выходит.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/logout", nil)
	req.Header.Set("X-Autobak", "1")
	req.AddCookie(c)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("logout с заголовком не сработал: код %d", rec.Code)
	}
	if s0valid(h, c) {
		t.Fatal("сессия жива после выхода")
	}
}

// s0valid проверяет, принимает ли сервер сессию (через любой API-вызов).
func s0valid(h http.Handler, c *http.Cookie) bool {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/State", strings.NewReader("[]"))
	req.Header.Set("X-Autobak", "1")
	req.AddCookie(c)
	h.ServeHTTP(rec, req)
	return rec.Code == http.StatusOK
}
