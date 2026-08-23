package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/iamtime/autobak/internal/uiapi"
	"github.com/iamtime/autobak/internal/webui"
)

type server struct {
	api  *uiapi.API
	auth *auth
	tls  bool
	// secure - обслуживать ли соединение как защищённое (кука Secure,
	// заголовок HSTS). Это не то же, что tls: TLS может терминировать
	// обратный прокси, а до нас доходить обычный HTTP. Тогда кука всё
	// равно должна быть Secure, иначе при навязанном http:// браузер
	// пришлёт сессию открытым текстом.
	secure bool
	// trustProxy разрешает доверять заголовку X-Forwarded-For при
	// определении адреса клиента. Включать только когда впереди
	// действительно стоит доверенный прокси: иначе клиент подделает адрес
	// и обойдёт защиту от подбора пароля.
	trustProxy bool

	hub *hub
}

type serverOptions struct {
	tls        bool
	secure     bool
	trustProxy bool
}

// notCallable - методы ядра, которые не должны становиться частью
// веб-API.
//
// Всё остальное экспортированное доступно намеренно: это ровно тот же
// набор, что видит окно через мост Wails. Разъезд двух списков означал
// бы, что в вебе чего-то нельзя, и об этом узнают в неподходящий момент.
var notCallable = []string{"App", "SetEmit"}

func newServer(api *uiapi.API, a *auth, opt serverOptions) *server {
	s := &server{
		api: api, auth: a,
		tls:        opt.tls,
		secure:     opt.tls || opt.secure,
		trustProxy: opt.trustProxy,
		hub:        newHub(),
	}
	api.SetEmit(s.emit)
	return s
}

// clientIP - адрес клиента для защиты от подбора пароля.
//
// За доверенным обратным прокси реальный адрес приходит в X-Forwarded-For,
// а RemoteAddr - это адрес самого прокси, один на всех. Без учёта XFF один
// злоумышленник заблокировал бы вход всем сразу. Доверяем заголовку только
// при явно включённом trustProxy: иначе клиент сам подставит любой адрес.
func (s *server) clientIP(r *http.Request) string {
	if s.trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			// Первый адрес в списке - исходный клиент.
			if i := strings.IndexByte(xff, ','); i >= 0 {
				return strings.TrimSpace(xff[:i])
			}
			return strings.TrimSpace(xff)
		}
	}
	return r.RemoteAddr
}

// emit рассылает событие вкладкам и заодно кладёт важное в журнал.
//
// Журнал переживает закрытую вкладку и виден в `docker logs`, а бэкап по
// расписанию идёт ночью, когда браузер никто не держит открытым.
func (s *server) emit(name string, data any) {
	s.hub.broadcast(name, data)
	if name != "log" {
		return
	}
	if l, ok := data.(uiapi.LogLine); ok && l.Level != "info" {
		log.Printf("[%s] %s", l.Level, l.Msg)
	}
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /login", s.handleLogin)
	mux.HandleFunc("GET /login", s.handleLoginPage)
	mux.HandleFunc("POST /logout", s.handleLogout)
	mux.HandleFunc("POST /api/{method}", s.requireAuth(s.handleAPI))
	mux.HandleFunc("GET /api/events", s.requireAuth(s.handleEvents))
	mux.Handle("GET /", s.requireAuthPage(http.FileServerFS(webui.Dist())))
	return s.withSecurityHeaders(mux)
}

// withSecurityHeaders выставляет заголовки, которые дёшевы и отсекают
// целые классы неприятностей.
func (s *server) withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		// Всё своё и встроено в бинарь, поэтому политика максимально
		// узкая: ни одного внешнего источника.
		h.Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self' 'unsafe-inline'; "+
				"style-src 'self' 'unsafe-inline'; img-src 'self' data:; "+
				"connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		if s.secure {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

func (s *server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.auth.valid(sessionOf(r)) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "требуется вход"})
			return
		}
		next(w, r)
	}
}

func (s *server) requireAuthPage(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.auth.valid(sessionOf(r)) {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if s.auth.valid(sessionOf(r)) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, loginPage(r.URL.Query().Get("e")))
}

func (s *server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if blocked, wait := s.auth.blocked(s.clientIP(r)); blocked {
		http.Redirect(w, r,
			"/login?e="+fmt.Sprintf("слишком много попыток, подождите %s",
				wait.Round(time.Second)), http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/login?e=некорректный запрос", http.StatusSeeOther)
		return
	}
	user := r.PostFormValue("user")
	pass := r.PostFormValue("password")

	if !s.auth.check(user, pass) {
		s.auth.failed(s.clientIP(r))
		// Пауза одинаковая для «нет такого пользователя» и «неверный
		// пароль»: разное время ответа подсказало бы, какое из двух.
		time.Sleep(loginDelay)
		log.Printf("неудачный вход с %s", hostOf(s.clientIP(r)))
		http.Redirect(w, r, "/login?e=неверный логин или пароль", http.StatusSeeOther)
		return
	}

	s.auth.succeeded(s.clientIP(r))
	token, err := s.auth.newSession()
	if err != nil {
		http.Error(w, "не удалось создать сессию", http.StatusInternalServerError)
		return
	}
	s.setSessionCookie(w, token, int(sessionTTL.Seconds()))
	log.Printf("вход выполнен с %s", hostOf(s.clientIP(r)))
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *server) handleLogout(w http.ResponseWriter, r *http.Request) {
	// Тот же служебный заголовок, что и в API: без него форма с чужого
	// сайта могла бы разлогинивать администратора по кругу. Кнопка выхода
	// в интерфейсе шлёт его через fetch.
	if r.Header.Get("X-Autobak") == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "запрос без служебного заголовка"})
		return
	}
	s.auth.drop(sessionOf(r))
	s.setSessionCookie(w, "", -1)
	writeJSON(w, http.StatusOK, map[string]string{"ok": "1"})
}

// handleAPI вызывает метод ядра по имени.
//
// Через отражение, а не по списку обработчиков: список пришлось бы
// дописывать при каждом новом методе, и однажды его забудут - в вебе
// не окажется того, что есть в окне.
func (s *server) handleAPI(w http.ResponseWriter, r *http.Request) {
	// Обязательный заголовок - вторая половина защиты от подделки
	// запросов: форма с чужого сайта его не поставит, а простой запрос
	// с ним браузер уже не отправит без разрешения.
	if r.Header.Get("X-Autobak") == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "запрос без служебного заголовка"})
		return
	}
	name := r.PathValue("method")
	if slices.Contains(notCallable, name) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "метод недоступен"})
		return
	}

	m := reflect.ValueOf(s.api).MethodByName(name)
	if !m.IsValid() {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "нет такого метода: " + name})
		return
	}

	var raw []json.RawMessage
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20))
	if err := dec.Decode(&raw); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "аргументы не разобрать: " + err.Error()})
		return
	}

	args, err := buildArgs(m.Type(), raw)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	out := m.Call(args)
	result, callErr := splitResults(out)
	if callErr != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": callErr.Error()})
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func buildArgs(t reflect.Type, raw []json.RawMessage) ([]reflect.Value, error) {
	if t.NumIn() != len(raw) {
		return nil, fmt.Errorf("ожидалось аргументов: %d, получено %d", t.NumIn(), len(raw))
	}
	args := make([]reflect.Value, t.NumIn())
	for i := range args {
		v := reflect.New(t.In(i))
		if err := json.Unmarshal(raw[i], v.Interface()); err != nil {
			return nil, fmt.Errorf("аргумент %d: %w", i+1, err)
		}
		args[i] = v.Elem()
	}
	return args, nil
}

// splitResults разбирает то, что вернул метод: (), (T), (error), (T, error).
func splitResults(out []reflect.Value) (any, error) {
	var result any
	var err error
	for _, v := range out {
		if v.Type() == reflect.TypeFor[error]() {
			if !v.IsNil() {
				err = v.Interface().(error)
			}
			continue
		}
		result = v.Interface()
	}
	return result, err
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v == nil {
		w.Write([]byte("null"))
		return
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("ответ не отправлен: %v", err)
	}
}

// --- События --------------------------------------------------------------

// hub рассылает события всем открытым вкладкам.
//
// Событий много (прогресс бэкапа), а подписчиков мало, поэтому каждому
// даётся буфер, и медленный подписчик теряет события, а не тормозит
// операцию. Терять здесь безопасно: события - это отображение хода
// работы, а не сама работа.
type hub struct {
	mu      sync.Mutex
	clients map[chan sseEvent]struct{}
}

type sseEvent struct {
	Name string
	Data []byte
}

func newHub() *hub { return &hub{clients: map[chan sseEvent]struct{}{}} }

func (h *hub) add() chan sseEvent {
	ch := make(chan sseEvent, 64)
	h.mu.Lock()
	h.clients[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *hub) remove(ch chan sseEvent) {
	h.mu.Lock()
	delete(h.clients, ch)
	h.mu.Unlock()
	close(ch)
}

func (h *hub) broadcast(name string, data any) {
	payload, err := json.Marshal(data)
	if err != nil {
		return
	}
	ev := sseEvent{Name: name, Data: payload}

	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.clients {
		select {
		case ch <- ev:
		default:
			// Вкладка не успевает читать. Пропускаем событие: подвесить
			// из-за неё бэкап было бы куда хуже.
		}
	}
}

func (s *server) handleEvents(w http.ResponseWriter, r *http.Request) {
	// HEAD страница использует как проверку «жива ли ещё сессия» перед
	// тем, как переподключаться после разрыва. Открывать ради этого
	// поток незачем: раз запрос сюда дошёл, сессия действительна.
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "поток событий не поддерживается", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	// Отключает буферизацию в nginx: без этого события копятся в прокси
	// и приходят пачкой в конце операции, когда они уже не нужны.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ch := s.hub.add()
	defer s.hub.remove(ch)

	// Периодический комментарий не даёт прокси и балансировщикам закрыть
	// соединение по бездействию во время долгого этапа без событий.
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case ev := <-ch:
			fmt.Fprintf(w, "data: {\"name\":%q,\"data\":%s}\n\n", ev.Name, ev.Data)
			flusher.Flush()
		}
	}
}

func loginPage(errMsg string) string {
	var banner string
	if errMsg != "" {
		banner = `<div class="err">` + htmlEscape(errMsg) + `</div>`
	}
	return `<!doctype html><html lang="ru"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>AutoBak</title><style>
:root{--bg:#0f1115;--panel:#16191f;--line:#262b35;--text:#e6e9ef;--muted:#8b93a3;
--accent:#4c8dff;--danger:#f2545b}
@media(prefers-color-scheme:light){:root{--bg:#f4f6f9;--panel:#fff;--line:#dfe3ea;
--text:#1a1d24;--muted:#6b7383;--accent:#2563eb}}
*{box-sizing:border-box}
body{margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center;
background:var(--bg);color:var(--text);font:14px/1.5 "Segoe UI",system-ui,sans-serif}
form{background:var(--panel);border:1px solid var(--line);border-radius:12px;
padding:26px 28px;width:min(360px,92vw)}
h1{margin:0 0 4px;font-size:19px}
p.sub{margin:0 0 18px;color:var(--muted);font-size:12.5px}
label{display:block;margin-bottom:12px}
label span{display:block;font-size:12px;color:var(--muted);margin-bottom:4px}
input{width:100%;background:var(--bg);color:var(--text);border:1px solid var(--line);
border-radius:6px;padding:8px 10px;font:inherit}
input:focus{outline:none;border-color:var(--accent)}
button{width:100%;margin-top:6px;background:var(--accent);color:#fff;border:0;
border-radius:6px;padding:9px;font:inherit;cursor:pointer}
.err{background:color-mix(in srgb,var(--danger) 15%,transparent);
border-left:3px solid var(--danger);padding:8px 10px;border-radius:0 6px 6px 0;
margin-bottom:14px;font-size:12.5px}
</style></head><body>
<form method="post" action="/login">
<h1>AutoBak</h1>
<p class="sub">резервное копирование серверов</p>
` + banner + `
<label><span>Пользователь</span><input name="user" autocomplete="username" autofocus></label>
<label><span>Пароль</span><input name="password" type="password" autocomplete="current-password"></label>
<button type="submit">Войти</button>
</form></body></html>`
}

func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;")
	return r.Replace(s)
}
