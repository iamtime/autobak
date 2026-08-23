package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"
)

// Вход в веб-интерфейс.
//
// Пользователь один: это инструмент администратора, а не многопользова-
// тельский сервис. Учётные записи с ролями здесь были бы видимостью
// разграничения - любой, кто вошёл, всё равно может восстановить любой
// сервер и прочитать любой бэкап.
//
// Пароль не хранится: при старте из него выводится хэш Argon2id, и
// сравниваются хэши. Разница практическая - дамп памяти процесса и
// список окружения не выдают пароль в открытом виде.

const (
	sessionCookie = "autobak_session"
	sessionTTL    = 12 * time.Hour
	// loginDelay - задержка ответа на неудачный вход.
	//
	// Не защита от подбора сама по себе, а способ сделать подбор
	// заметным: тысяча попыток в секунду превращается в тысячу секунд.
	loginDelay = 700 * time.Millisecond
)

type auth struct {
	user string
	// hash и salt - от заданного пароля. Сам пароль не сохраняется.
	hash []byte
	salt []byte

	mu       sync.Mutex
	sessions map[string]time.Time
	// attempts считает неудачные попытки по адресу источника.
	attempts map[string]*attemptRecord
}

type attemptRecord struct {
	count int
	// last - время последней неудачи. Именно по нему решается, продолжается
	// подбор или это новый человек с новой опечаткой.
	last time.Time
	// until - до какого момента этому адресу отвечать отказом сразу.
	until time.Time
}

const (
	// attemptWindow - сколько тишины обнуляет счётчик. Час: наказание не
	// должно быть вечным, иначе один опечатавшийся человек лишится
	// доступа к своим бэкапам до перезапуска.
	attemptWindow = time.Hour
	// attemptsMax - предел размера таблицы попыток. Подбор с меняющихся
	// адресов иначе растил бы её без границы.
	attemptsMax = 4096
)

func newAuth(user, password string) (*auth, error) {
	if user == "" {
		user = "admin"
	}
	if len(password) < 8 {
		return nil, errors.New("пароль короче 8 символов - это не пароль")
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	return &auth{
		user:     user,
		salt:     salt,
		hash:     argon2.IDKey([]byte(password), salt, 3, 64*1024, 4, 32),
		sessions: map[string]time.Time{},
		attempts: map[string]*attemptRecord{},
	}, nil
}

// check сверяет учётные данные.
//
// Сравнение постоянного времени: обычное сравнение по байтам выдаёт
// длину совпавшего префикса временем ответа.
func (a *auth) check(user, password string) bool {
	got := argon2.IDKey([]byte(password), a.salt, 3, 64*1024, 4, 32)
	okUser := subtle.ConstantTimeCompare([]byte(user), []byte(a.user)) == 1
	okPass := subtle.ConstantTimeCompare(got, a.hash) == 1
	return okUser && okPass
}

// blocked сообщает, не пора ли этому источнику подождать.
func (a *auth) blocked(remote string) (bool, time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()
	rec := a.attempts[hostOf(remote)]
	if rec == nil || time.Now().After(rec.until) {
		return false, 0
	}
	return true, time.Until(rec.until)
}

func (a *auth) failed(remote string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	key := hostOf(remote)
	rec := a.attempts[key]
	// Сбрасываем счётчик только после часа тишины. Сравнение с rec.until,
	// как было раньше, обнуляло его на каждой попытке: у свежей записи
	// until нулевой, то есть уже в прошлом, и наказание не наступало
	// никогда - подбор шёл с полной скоростью.
	if rec == nil || time.Since(rec.last) > attemptWindow {
		rec = &attemptRecord{}
		a.attempts[key] = rec
	}
	rec.count++
	rec.last = time.Now()
	a.forgetOldAttempts()
	// Пауза растёт с числом попыток: человек, ошибшийся дважды, почти не
	// заметит, а перебор быстро упрётся в минуты ожидания.
	if rec.count >= 5 {
		delay := time.Duration(rec.count-4) * 30 * time.Second
		if delay > 15*time.Minute {
			delay = 15 * time.Minute
		}
		rec.until = time.Now().Add(delay)
	}
}

func (a *auth) succeeded(remote string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.attempts, hostOf(remote))
}

// forgetOldAttempts не даёт таблице расти без предела. Вызывается под замком.
func (a *auth) forgetOldAttempts() {
	if len(a.attempts) <= attemptsMax {
		return
	}
	for k, rec := range a.attempts {
		if time.Since(rec.last) > attemptWindow {
			delete(a.attempts, k)
		}
	}
}

func hostOf(remote string) string {
	if h, _, err := net.SplitHostPort(remote); err == nil {
		return h
	}
	return remote
}

// newSession выдаёт токен сессии.
func (a *auth) newSession() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)

	a.mu.Lock()
	defer a.mu.Unlock()
	a.sessions[token] = time.Now().Add(sessionTTL)
	a.sweep()
	return token, nil
}

// valid проверяет сессию и продлевает её.
func (a *auth) valid(token string) bool {
	if token == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	exp, ok := a.sessions[token]
	if !ok || time.Now().After(exp) {
		delete(a.sessions, token)
		return false
	}
	// Продление при активности: администратора не должно выбрасывать
	// посреди восстановления только потому, что оно идёт третий час.
	a.sessions[token] = time.Now().Add(sessionTTL)
	return true
}

func (a *auth) drop(token string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.sessions, token)
}

// sweep убирает просроченные сессии. Вызывается под замком.
func (a *auth) sweep() {
	now := time.Now()
	for t, exp := range a.sessions {
		if now.After(exp) {
			delete(a.sessions, t)
		}
	}
}

func (s *server) setSessionCookie(w http.ResponseWriter, token string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:  sessionCookie,
		Value: token,
		Path:  "/",
		// HttpOnly: даже найденная в интерфейсе уязвимость с внедрением
		// скрипта не должна отдавать сессию наружу.
		HttpOnly: true,
		// SameSite=Strict - основная защита от подделки запросов:
		// браузер не приложит эту куку к запросу, пришедшему с чужого
		// сайта. Вместе с обязательным заголовком в API этого достаточно.
		SameSite: http.SameSiteStrictMode,
		Secure:   s.secure,
		MaxAge:   maxAge,
	})
}

func sessionOf(r *http.Request) string {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return ""
	}
	return c.Value
}

// randomPassword выдаёт пароль для первого запуска, когда его не задали.
//
// Печатается в журнал один раз. Вариант «пустой пароль по умолчанию»
// не рассматривается: такие установки живут годами и находятся сканерами
// за часы.
func randomPassword() string {
	raw := make([]byte, 12)
	if _, err := rand.Read(raw); err != nil {
		panic("autobak: нет источника случайности: " + err.Error())
	}
	s := hex.EncodeToString(raw)
	var parts []string
	for i := 0; i < len(s); i += 6 {
		parts = append(parts, s[i:min(i+6, len(s))])
	}
	return strings.Join(parts, "-")
}
