package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/iamtime/autobak/internal/backend"
	"github.com/iamtime/autobak/internal/discover"
	"github.com/iamtime/autobak/internal/engine"
	"github.com/iamtime/autobak/internal/gitmirror"
	"github.com/iamtime/autobak/internal/notify"
	"github.com/iamtime/autobak/internal/proto"
	"github.com/iamtime/autobak/internal/repo"
	"github.com/iamtime/autobak/internal/restore"
	"github.com/iamtime/autobak/internal/secretstore"
)

var Version = "dev"

type App struct {
	mu      sync.Mutex
	dir     string
	cfgPath string

	// cfgMu защищает конфигурацию целиком.
	//
	// Нужен потому, что долгая операция дописывает в неё итог (s.Last),
	// пока интерфейс её читает. В окне это почти безобидно - действия
	// там выполняются по одному, - но веб-сервер обслуживает запросы
	// параллельно, и там это настоящая гонка.
	cfgMu   sync.RWMutex
	cfg     *Config
	secrets *secretstore.Store

	// openRepos кэширует открытые репозитории: подъём индекса на сотню
	// тысяч чанков занимает секунды, и делать это на каждое действие
	// интерфейса нельзя.
	openRepos map[string]*repo.Repo

	// staleNotified - когда последний раз уведомляли о «протухшем» сервере.
	// Без этого веб-планировщик, проверяющий каждые 10 минут, слал бы
	// уведомление о сбое на каждом тике - десятки писем в час.
	staleMu       sync.Mutex
	staleNotified map[string]time.Time
}

func Open() (*App, error) {
	dir, err := ConfigDir()
	if err != nil {
		return nil, err
	}
	return OpenAt(dir)
}

func OpenAt(dir string) (*App, error) {
	cfgPath := filepath.Join(dir, "config.json")
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return nil, err
	}
	return &App{
		dir: dir, cfgPath: cfgPath, cfg: cfg,
		secrets:       secretstore.New(filepath.Join(dir, "secrets.dat")),
		openRepos:     map[string]*repo.Repo{},
		staleNotified: map[string]time.Time{},
	}, nil
}

// Config отдаёт конфигурацию напрямую. Годится для чтения из одного
// потока (командная строка); всё остальное обязано идти через Read
// и Update, иначе гонка.
func (a *App) Config() *Config { return a.cfg }
func (a *App) Dir() string     { return a.dir }

// Read выполняет чтение конфигурации под общей блокировкой.
func (a *App) Read(fn func(*Config)) {
	a.cfgMu.RLock()
	defer a.cfgMu.RUnlock()
	fn(a.cfg)
}

// Update меняет конфигурацию и сразу сохраняет её.
//
// Изменение и запись под одной блокировкой намеренно: иначе два
// одновременных изменения записали бы файл дважды, и второй затёр бы
// первое.
func (a *App) Update(fn func(*Config) error) error {
	a.cfgMu.Lock()
	defer a.cfgMu.Unlock()
	if err := fn(a.cfg); err != nil {
		return err
	}
	return a.saveLocked()
}

func (a *App) Save() error {
	a.cfgMu.Lock()
	defer a.cfgMu.Unlock()
	return a.saveLocked()
}

// saveLocked пишет конфигурацию под межпроцессной блокировкой. Вызывается
// под cfgMu (внутрипроцессная сериализация); файловая блокировка добавляет
// защиту от другого процесса, делящего тот же каталог настроек.
func (a *App) saveLocked() error {
	lock, err := lockConfig(a.dir)
	if err != nil {
		return err
	}
	defer lock.unlock()
	return saveConfig(a.cfgPath, a.cfg)
}

func newID() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		// Единственный источник случайности недоступен - продолжать нельзя:
		// совпавшие идентификаторы означали бы перепутанные репозитории.
		panic("autobak: нет источника случайности: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// --- Репозитории ----------------------------------------------------------

// AddRepo регистрирует хранилище и создаёт в нём репозиторий, если его
// там ещё нет. Возвращает recovery-код при создании нового.
func (a *App) AddRepo(ctx context.Context, r *Repo, secretKey, password string) (recovery string, err error) {
	if r.Name == "" {
		return "", errors.New("autobak: у репозитория должно быть имя")
	}
	if password == "" {
		return "", errors.New("autobak: репозиторий без пароля не создаётся")
	}
	r.ID = newID()
	r.Created = time.Now()

	if secretKey != "" {
		if err := a.secrets.Set(r.secretKeyID(), secretKey); err != nil {
			return "", err
		}
	}
	if err := a.secrets.Set(r.passwordKeyID(), password); err != nil {
		return "", err
	}

	be, err := a.backendFor(r, backend.Caps{CanWrite: true, CanDelete: true})
	if err != nil {
		return "", err
	}
	defer be.Close()

	if _, err := repo.Open(ctx, be, password); err == nil {
		// Уже существующий репозиторий просто подключаем: так десктоп
		// на новом компьютере получает всю историю обратно.
		return "", a.Update(func(c *Config) error {
			c.Repos = append(c.Repos, r)
			return nil
		})
	} else if !errors.Is(err, repo.ErrNotInitialized) && !errors.Is(err, repo.ErrNoKey) {
		return "", err
	}

	_, recovery, err = repo.Init(ctx, be, password, r.Name)
	if err != nil {
		return "", err
	}
	return recovery, a.Update(func(c *Config) error {
		c.Repos = append(c.Repos, r)
		return nil
	})
}

func (a *App) backendFor(r *Repo, caps backend.Caps) (backend.Backend, error) {
	switch r.Kind {
	case RepoLocal:
		return backend.OpenLocal(r.Path, caps)
	case RepoS3:
		secret, err := a.secrets.Get(r.secretKeyID())
		if err != nil {
			return nil, fmt.Errorf("autobak: нет сохранённого ключа для %s: %w", r.Name, err)
		}
		return backend.OpenS3(backend.S3Config{
			Endpoint: r.Endpoint, Region: r.Region, Bucket: r.Bucket, Prefix: r.Prefix,
			AccessKey: r.AccessKey, SecretKey: secret, PathStyle: r.PathStyle, Caps: caps,
		})
	}
	return nil, fmt.Errorf("autobak: неизвестный тип хранилища %q", r.Kind)
}

// OpenRepo открывает репозиторий и запоминает его.
func (a *App) OpenRepo(ctx context.Context, repoID string) (*repo.Repo, error) {
	a.mu.Lock()
	if r, ok := a.openRepos[repoID]; ok {
		a.mu.Unlock()
		return r, nil
	}
	a.mu.Unlock()

	cr, err := a.cfg.Repo(repoID)
	if err != nil {
		return nil, err
	}
	password, err := a.secrets.Get(cr.passwordKeyID())
	if err != nil {
		return nil, fmt.Errorf("autobak: нет сохранённого пароля для %s: %w", cr.Name, err)
	}
	be, err := a.backendFor(cr, backend.Caps{CanWrite: true, CanDelete: true})
	if err != nil {
		return nil, err
	}
	r, err := repo.Open(ctx, be, password)
	if err != nil {
		be.Close()
		return nil, err
	}

	a.mu.Lock()
	a.openRepos[cr.ID] = r
	a.mu.Unlock()
	return r, nil
}

// CloseRepos освобождает открытые соединения. Нужен после prune, чтобы
// следующее открытие перечитало индекс.
func (a *App) CloseRepos() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for id, r := range a.openRepos {
		r.Backend().Close()
		delete(a.openRepos, id)
	}
}

// --- Серверы --------------------------------------------------------------

func (a *App) AddServer(s *Server) error {
	if s.Name == "" {
		return errors.New("autobak: у сервера должно быть имя")
	}
	if _, err := a.cfg.Repo(s.RepoID); err != nil {
		return err
	}
	s.ID = newID()
	if s.Mode == "" {
		s.Mode = ModePull
	}
	if s.Retention == (repo.Retention{}) {
		s.Retention = repo.DefaultRetention()
	}
	return a.Update(func(c *Config) error {
		c.Servers = append(c.Servers, s)
		return nil
	})
}

// Discover опрашивает сервер и возвращает его карту.
func (a *App) Discover(ctx context.Context, serverID string) (*discover.Report, error) {
	s, err := a.cfg.Server(serverID)
	if err != nil {
		return nil, err
	}
	res, err := s.SSH.RunAgent(ctx, 3*time.Minute, "discover", "--json")
	if err != nil {
		return nil, err
	}
	var rep discover.Report
	if err := json.Unmarshal([]byte(res.Stdout), &rep); err != nil {
		return nil, fmt.Errorf("autobak: агент вернул неожиданный ответ: %w", err)
	}
	return &rep, nil
}

// Estimate - предварительная оценка объёма бэкапа.
type Estimate struct {
	Bytes   int64    `json:"bytes"`
	Files   int64    `json:"files"`
	Partial bool     `json:"partial"`
	Skipped []string `json:"skipped,omitempty"`
	// LastStored - сколько занял прошлый успешный бэкап этого сервера в
	// хранилище (0 - бэкапов ещё не было). Лучший ориентир по месту:
	// оценка выше - это «сколько читать», а реально на диск ляжет меньше.
	LastStored int64 `json:"last_stored"`
}

// EstimateBackup спрашивает у сервера, сколько данных уйдёт в бэкап по
// текущему плану, с учётом исключений. Обход только по метаданным - быстро.
func (a *App) EstimateBackup(ctx context.Context, serverID string) (*Estimate, error) {
	s, err := a.cfg.Server(serverID)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(s.Plan)
	if err != nil {
		return nil, err
	}
	res, err := s.SSH.RunAgentInput(ctx, 5*time.Minute, bytes.NewReader(raw), "estimate")
	if err != nil {
		return nil, err
	}
	var est Estimate
	if err := json.Unmarshal([]byte(res.Stdout), &est); err != nil {
		return nil, fmt.Errorf("autobak: агент вернул неожиданный ответ: %w", err)
	}
	est.LastStored = s.Last.GoodStored
	return &est, nil
}

// Events - то, что интерфейс показывает во время длинной операции.
type Events struct {
	Progress func(proto.Progress)
	Log      func(level, msg string)
}

func (e Events) log(level, msg string) {
	if e.Log != nil {
		e.Log(level, msg)
	}
}

// Backup выполняет бэкап сервера.
//
// В pull-режиме десктоп запускает агента по SSH, принимает поток и сам
// шифрует данные. В push-режиме он лишь даёт команду: агент пишет в
// хранилище напрямую, а десктоп смотрит за журналом.
func (a *App) Backup(ctx context.Context, serverID string, ev Events) (*repo.Snapshot, error) {
	s, err := a.cfg.Server(serverID)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	snap, err := a.runBackup(ctx, s, ev)

	last := LastRun{Time: start, Duration: time.Since(start).Round(time.Second).String()}
	// Данные о последнем удачном бэкапе переносятся из предыдущего запуска:
	// они должны пережить неудачу, чтобы не рвалась родословная снимков и
	// не терялось время последнего успеха.
	last.GoodTime = s.Last.GoodTime
	last.GoodSnapshotID = s.Last.GoodSnapshotID
	last.GoodBytes = s.Last.GoodBytes
	last.GoodStored = s.Last.GoodStored
	if err != nil {
		last.Error = err.Error()
		_ = a.Update(func(*Config) error { s.Last = last; return nil })
		a.notifyAbout(ctx, notify.Message{
			Level: notify.LevelError, Server: s.Name,
			Title: "бэкап не выполнен",
			Body:  err.Error(),
		}, ev)
		return nil, err
	}
	last.OK = true
	last.SnapshotID = snap.ID
	last.Bytes = snap.Stats.BytesTotal
	last.Partial = !snap.Complete()
	// Удачный бэкап обновляет и «последний хороший».
	last.GoodTime = start
	last.GoodSnapshotID = snap.ID
	last.GoodBytes = snap.Stats.BytesTotal
	last.GoodStored = snap.Stats.BytesStored
	if err := a.Update(func(*Config) error { s.Last = last; return nil }); err != nil {
		return snap, err
	}
	a.reportBackup(ctx, s, snap, ev)

	if s.Git.Enabled {
		// Ошибка зеркала не отменяет успешный бэкап: git - дополнительный
		// канал, а не хранилище, и его недоступность не потеря данных.
		cfg := s.Git
		cfg.Token, _ = a.secrets.Get("git/" + s.ID + "/token")
		cfg.Log = ev.Log
		if cfg.WorkDir == "" {
			cfg.WorkDir = filepath.Join(a.dir, "gitmirror")
		}
		r, err := a.OpenRepo(ctx, s.RepoID)
		if err == nil {
			rep, gerr := gitmirror.Sync(ctx, r, snap, cfg)
			if gerr != nil {
				ev.log("warn", "git-зеркало: "+gerr.Error())
			} else {
				ev.log("info", "git-зеркало: "+rep.Summary())
			}
		}
	}
	return snap, nil
}

func (a *App) runBackup(ctx context.Context, s *Server, ev Events) (*repo.Snapshot, error) {
	if err := s.Plan.Validate(); err != nil {
		return nil, err
	}
	if s.Mode == ModePush {
		return a.runPushBackup(ctx, s, ev)
	}

	r, err := a.OpenRepo(ctx, s.RepoID)
	if err != nil {
		return nil, err
	}

	cmd, err := s.SSH.Agent(ctx, "export")
	if err != nil {
		return nil, err
	}
	planJSON, err := json.Marshal(s.Plan)
	if err != nil {
		return nil, err
	}

	// Известные чанки этого сервера. По ним агент поймёт, что передавать
	// повторно не нужно, и по сети поедет только изменившееся.
	known, err := a.knownChunks(ctx, r, s)
	if err != nil {
		return nil, err
	}

	req := proto.Request{
		Plan:     planJSON,
		ChunkKey: hex.EncodeToString(r.Key().ChunkIDKey()),
		Chunker:  r.Chunker(),
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("autobak: не запустить агента на %s: %w", s.SSH.Label(), err)
	}
	// Запрос уходит кадром, а канал остаётся открытым: по нему пойдут
	// ответы о том, какие чанки у нас уже есть.
	reqW := proto.NewWriter(stdin)
	if err := reqW.JSON(proto.FrameRequest, req); err != nil {
		cmd.Process.Kill()
		return nil, err
	}
	if err := reqW.Flush(); err != nil {
		cmd.Process.Kill()
		return nil, err
	}
	defer stdin.Close()
	// stderr агента - это диагностика ssh и самого агента. Читаем его
	// параллельно: если не читать, труба переполнится и агент встанет.
	stderrDone := make(chan string, 1)
	go func() { stderrDone <- readTail(stderr, 8<<10) }()

	snap, importErr := engine.Import(ctx, r, stdout, stdin, engine.Options{
		Server: s.Name, Agent: Version, Parent: s.Last.GoodSnapshotID,
		Known:    known,
		Progress: ev.Progress, Log: ev.Log,
	})
	if importErr != nil {
		// Агент в этот момент почти наверняка пишет в поток, который мы
		// перестали читать. Труба заполнится, он встанет на записи, а мы
		// встанем на ожидании его завершения - и бэкап повиснет навсегда.
		// Поэтому сначала прекращаем процесс, и только потом ждём.
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}
	waitErr := cmd.Wait()
	diag := <-stderrDone

	if importErr != nil {
		if diag != "" {
			return nil, fmt.Errorf("%w (агент сообщил: %s)", importErr, diag)
		}
		return nil, importErr
	}
	if waitErr != nil {
		return nil, fmt.Errorf("autobak: агент завершился с ошибкой: %w: %s", waitErr, diag)
	}
	return snap, nil
}

// runPushBackup только даёт команду: данные идут с сервера в хранилище
// напрямую, минуя этот компьютер.
func (a *App) runPushBackup(ctx context.Context, s *Server, ev Events) (*repo.Snapshot, error) {
	res, err := s.SSH.RunAgent(ctx, 12*time.Hour, "backup", "--config", "/etc/autobak/config.json")
	if res != nil && res.Stderr != "" {
		ev.log("info", res.Stderr)
	}
	if err != nil {
		return nil, err
	}
	// Снимок сделал агент - забираем его из репозитория, чтобы показать
	// в интерфейсе тот же объект, что и в pull-режиме.
	r, err := a.OpenRepo(ctx, s.RepoID)
	if err != nil {
		return nil, err
	}
	snaps, err := r.ListSnapshots(ctx)
	if err != nil {
		return nil, err
	}
	for _, sn := range snaps {
		if sn.Server == s.Name {
			return sn, nil
		}
	}
	return nil, errors.New("autobak: агент отчитался об успехе, но снимок в репозитории не найден")
}

// lineBreak - разделитель строк в теле уведомления.
const lineBreak = "\n"

// notifyConfig достаёт настройки уведомлений вместе с секретами.
func (a *App) notifyConfig() notify.Config {
	var c notify.Config
	a.Read(func(cfg *Config) { c = cfg.Notify })
	c.Telegram.Token, _ = a.secrets.Get("notify/telegram_token")
	c.Mail.Pass, _ = a.secrets.Get("notify/mail_pass")
	return c
}

// notifyAbout отправляет сообщение, не превращая сбой доставки в сбой
// операции: недоступный Telegram не должен выглядеть как проваленный бэкап.
func (a *App) notifyAbout(ctx context.Context, m notify.Message, ev Events) {
	if err := notify.Send(ctx, a.notifyConfig(), m); err != nil {
		ev.log("warn", err.Error())
	}
}

// reportBackup решает, о чём сообщать после удачного бэкапа.
//
// Неполный снимок - отдельное состояние: в списке он выглядит обычным,
// а при восстановлении окажется без баз. Молчать о таком нельзя.
func (a *App) reportBackup(ctx context.Context, s *Server, snap *repo.Snapshot, ev Events) {
	cfg := a.notifyConfig()
	if !snap.Complete() {
		var parts []string
		for _, m := range snap.Failed() {
			parts = append(parts, m.Name+": "+m.Err)
		}
		a.notifyAbout(ctx, notify.Message{
			Level: notify.LevelWarning, Server: s.Name,
			Title: "снимок сделан, но неполон",
			Body:  strings.Join(parts, lineBreak),
		}, ev)
		return
	}
	a.notifyAbout(ctx, notify.Message{
		Level: notify.LevelInfo, Server: s.Name,
		Title: "бэкап выполнен",
		Body: fmt.Sprintf("Снимок %s: файлов %d, данных %s, по сети %s.",
			snap.ID, snap.Stats.Files,
			repo.HumanBytes(snap.Stats.BytesTotal),
			repo.HumanBytes(snap.Stats.BytesWire)),
	}, ev)

	// Сигнал «я жив» шлётся только после полностью удачного бэкапа:
	// иначе внешняя служба будет считать, что всё в порядке, когда
	// половина модулей падает.
	if err := notify.Heartbeat(ctx, cfg.HeartbeatURL); err != nil {
		ev.log("warn", "сигнал «я жив» не доставлен: "+err.Error())
	}
}

// CheckStale сообщает о серверах, которые давно не бэкапились.
//
// Это единственная проверка, ловящая самый неприятный случай: задание
// не запускалось вовсе. Ошибки при этом не возникает - не возникает
// ничего, и уведомления об ошибках молчат.
func (a *App) CheckStale(ctx context.Context, ev Events) []string {
	cfg := a.notifyConfig()
	after := time.Duration(cfg.StaleAfter)
	if after <= 0 {
		return nil
	}
	var servers []*Server
	a.Read(func(c *Config) { servers = append(servers, c.Servers...) })

	var stale []string
	for _, s := range servers {
		if !s.Schedule.Enabled {
			continue
		}
		// Считаем по времени последнего УСПЕХА, а не последнего запуска:
		// сервер, падающий каждую ночь, обязан считаться протухшим, хотя
		// «последняя попытка» у него свежая.
		lastGood := s.Last.GoodTime
		if !lastGood.IsZero() && time.Since(lastGood) <= after {
			continue
		}
		if lastGood.IsZero() && s.Last.Time.IsZero() {
			// Сервер вообще ни разу не запускался: это не «протух», а «ещё
			// не начинал». О таком сообщать как о сбое рано.
			continue
		}
		stale = append(stale, s.Name)
		if a.shouldNotifyStale(s.Name, after) {
			a.notifyAbout(ctx, notify.Stale(s.Name, lastGood, after), ev)
		}
	}
	return stale
}

// shouldNotifyStale не даёт слать уведомление о протухшем сервере чаще
// одного раза за период StaleAfter: иначе проверка каждые 10 минут
// превратилась бы в поток одинаковых сообщений о сбое.
func (a *App) shouldNotifyStale(server string, after time.Duration) bool {
	a.staleMu.Lock()
	defer a.staleMu.Unlock()
	last, ok := a.staleNotified[server]
	if ok && time.Since(last) < after {
		return false
	}
	a.staleNotified[server] = time.Now()
	return true
}

// knownChunks собирает чанки, которые этот сервер уже присылал.
//
// Именно этого сервера, а не всего репозитория: если в одном хранилище
// живут несколько машин, ответ «этот чанк у меня есть» про чужой чанк
// позволил бы взломанному серверу сослаться на него и вытащить чужие
// данные ближайшим восстановлением.
//
// Берутся два последних снимка: одного мало, если предыдущий оказался
// неполным из-за сбоя модуля.
func (a *App) knownChunks(ctx context.Context, r *repo.Repo, s *Server) (map[repo.ChunkID]struct{}, error) {
	all, err := r.ListSnapshots(ctx)
	if err != nil {
		return nil, err
	}
	known := map[repo.ChunkID]struct{}{}
	used := 0
	for _, sn := range all {
		if sn.Server != s.Name || used >= 2 {
			continue
		}
		used++
		for _, id := range sn.Tree {
			known[id] = struct{}{}
		}
		if err := r.ReadTree(ctx, sn, func(n *repo.Node) error {
			for _, id := range n.Chunks {
				known[id] = struct{}{}
			}
			return nil
		}); err != nil {
			// Нечитаемый прошлый снимок - не повод отменять новый бэкап.
			// Просто передадим больше, чем могли бы.
			return known, nil
		}
	}
	return known, nil
}

func readTail(r io.Reader, limit int) string {
	data, _ := io.ReadAll(io.LimitReader(r, int64(limit)))
	return string(data)
}

// --- Снимки ---------------------------------------------------------------

func (a *App) Snapshots(ctx context.Context, serverID string) ([]*repo.Snapshot, error) {
	s, err := a.cfg.Server(serverID)
	if err != nil {
		return nil, err
	}
	r, err := a.OpenRepo(ctx, s.RepoID)
	if err != nil {
		return nil, err
	}
	all, err := r.ListSnapshots(ctx)
	if err != nil {
		return nil, err
	}
	var mine []*repo.Snapshot
	for _, sn := range all {
		if sn.Server == s.Name {
			mine = append(mine, sn)
		}
	}
	return mine, nil
}

// --- Восстановление -------------------------------------------------------

type RestoreOptions struct {
	SnapshotID string
	Include    []string
	Modules    []string

	// ToServer - восстановить обратно на сервер. Иначе данные скачиваются
	// в LocalDir на этот компьютер.
	ToServer bool
	LocalDir string

	// Overwrite и DBInPlace - необратимые действия. Оба по умолчанию
	// выключены, и интерфейс обязан запрашивать подтверждение вводом
	// имени сервера, прежде чем включить любой из них.
	Overwrite bool
	DBMode    restore.DBMode
	DBInPlace bool

	DryRun bool
}

func (a *App) Restore(ctx context.Context, serverID string, opt RestoreOptions, ev Events) (*restore.Report, error) {
	s, err := a.cfg.Server(serverID)
	if err != nil {
		return nil, err
	}
	r, err := a.OpenRepo(ctx, s.RepoID)
	if err != nil {
		return nil, err
	}
	snap, err := r.LoadSnapshot(ctx, opt.SnapshotID)
	if err != nil {
		return nil, err
	}

	ropt := restore.Options{
		Include: opt.Include, Modules: opt.Modules, DryRun: opt.DryRun,
		Log: ev.Log,
		Progress: func(done, total int64, path string) {
			if ev.Progress != nil {
				ev.Progress(proto.Progress{
					Stage: "восстановление", Path: path, Bytes: done, BytesTotal: total,
				})
			}
		},
	}

	if !opt.ToServer {
		dir := opt.LocalDir
		if dir == "" {
			dir = filepath.Join(a.dir, "restore", snap.ID)
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		ev.log("info", "восстановление в "+dir)
		return restore.Run(ctx, r, snap, ropt, restore.NewFS(restore.FSOptions{
			Root: dir, Overwrite: opt.Overwrite, Log: ev.Log,
		}))
	}

	// Сухой прогон на сервер выполняем локально: состав снимка (файлы,
	// объём, базы) посчитать можно и не трогая сервер. Но список
	// перезаписи так не получить - на локальном пустом каталоге его не
	// видно, а показать «перезаписей нет» перед восстановлением поверх
	// боевого сервера было бы опасной ложью. Помечаем отчёт честно:
	// перезаписи неизвестны, и подтверждение всё равно требует набрать
	// имя сервера.
	if opt.DryRun {
		rep, err := restore.Run(ctx, r, snap, ropt, restore.NewFS(restore.FSOptions{
			Root: filepath.Join(os.TempDir(), "autobak-dryrun"), Log: ev.Log,
		}))
		if err != nil {
			return nil, err
		}
		rep.Overwrites = nil
		rep.OverwriteUnknown = true
		return rep, nil
	}

	args := []string{"import", "--owner"}
	if opt.Overwrite {
		args = append(args, "--overwrite")
	}
	dbMode := opt.DBMode
	if dbMode == "" {
		dbMode = restore.DBFile
	}
	args = append(args, "--db="+string(dbMode))
	if dbMode == restore.DBRestore {
		if opt.DBInPlace {
			args = append(args, "--db-in-place")
		} else {
			args = append(args, "--db-suffix=_restore_"+time.Now().Format("20060102"))
		}
	}

	cmd, err := s.SSH.Agent(ctx, args...)
	if err != nil {
		return nil, err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	stderrDone := make(chan string, 1)
	go func() { stderrDone <- readTail(stderr, 16<<10) }()

	rep, runErr := restore.Run(ctx, r, snap, ropt, restore.NewProto(stdin))
	stdin.Close()
	waitErr := cmd.Wait()
	diag := <-stderrDone
	if diag != "" {
		ev.log("info", diag)
	}
	if runErr != nil {
		return rep, runErr
	}
	if waitErr != nil {
		return rep, fmt.Errorf("autobak: агент не смог применить восстановление: %w: %s", waitErr, diag)
	}
	return rep, nil
}

// --- Обслуживание ---------------------------------------------------------

func (a *App) Prune(ctx context.Context, serverID string, dryRun bool, ev Events) (*repo.PruneReport, error) {
	s, err := a.cfg.Server(serverID)
	if err != nil {
		return nil, err
	}
	r, err := a.OpenRepo(ctx, s.RepoID)
	if err != nil {
		return nil, err
	}
	opt := repo.DefaultPruneOptions()
	opt.Policy = s.Retention
	opt.DryRun = dryRun
	// Очистка ограничена снимками этого сервера: в общем репозитории
	// политика одного сервера не должна выкашивать историю другого.
	// Снимок хранит имя сервера (Snapshot.Server = s.Name), по нему же
	// группирует retention - фильтруем по тому же ключу.
	opt.Server = s.Name
	opt.Progress = func(stage string, done, total int) {
		if ev.Progress != nil {
			ev.Progress(proto.Progress{Stage: stage, Files: int64(done), BytesTotal: int64(total)})
		}
	}
	rep, err := r.Prune(ctx, opt)
	if err == nil && !dryRun {
		a.CloseRepos() // индекс изменился, соединение переоткроем
	}
	return rep, err
}

func (a *App) Verify(ctx context.Context, repoID string, sample float64, ev Events) (*repo.VerifyReport, error) {
	r, err := a.OpenRepo(ctx, repoID)
	if err != nil {
		return nil, err
	}
	return r.Verify(ctx, repo.VerifyOptions{
		Sample: sample,
		Progress: func(done, total int) {
			if ev.Progress != nil {
				ev.Progress(proto.Progress{Stage: "проверка", Files: int64(done), BytesTotal: int64(total)})
			}
		},
	})
}

// --- Зеркало и проверка восстановлением -----------------------------------

// Mirror копирует репозиторий в другое хранилище.
func (a *App) Mirror(ctx context.Context, fromID, toID string,
	opt repo.MirrorOptions, ev Events) (*repo.MirrorReport, error) {

	from, err := a.cfg.Repo(fromID)
	if err != nil {
		return nil, err
	}
	to, err := a.cfg.Repo(toID)
	if err != nil {
		return nil, err
	}
	if from.ID == to.ID {
		return nil, errors.New("autobak: зеркалировать репозиторий в самого себя нельзя")
	}

	src, err := a.backendFor(from, backend.Caps{})
	if err != nil {
		return nil, err
	}
	defer src.Close()
	// Право удаления выдаётся зеркалу только когда его действительно
	// просили чистить: без этого случайный prune не сможет распространиться
	// на копию, ради которой всё и затевалось.
	dst, err := a.backendFor(to, backend.Caps{CanWrite: true, CanDelete: opt.Prune})
	if err != nil {
		return nil, err
	}
	defer dst.Close()

	opt.Progress = func(stage string, done, total int) {
		if ev.Progress != nil {
			ev.Progress(proto.Progress{
				Stage: "зеркало: " + stage, Files: int64(done), BytesTotal: int64(total),
			})
		}
	}
	rep, err := repo.Mirror(ctx, src, dst, opt)
	if err != nil {
		return rep, err
	}
	if !opt.DryRun && rep.Copied > 0 {
		ev.log("info", "зеркало "+to.Name+": "+rep.Summary())
	}
	return rep, nil
}

// Drill восстанавливает выборку из снимка и сверяет её с записанным.
//
// Отметка о проверке хранится у репозитория: смысл в том, чтобы знать,
// когда хранилище в последний раз доказало свою пригодность, а не когда
// оно в последний раз принимало запись.
func (a *App) Drill(ctx context.Context, serverID, snapshotID string,
	opt restore.DrillOptions, ev Events) (*restore.DrillReport, error) {

	s, err := a.cfg.Server(serverID)
	if err != nil {
		return nil, err
	}
	r, err := a.OpenRepo(ctx, s.RepoID)
	if err != nil {
		return nil, err
	}
	snap, err := a.pickSnapshot(ctx, r, s.Name, snapshotID)
	if err != nil {
		return nil, err
	}

	opt.Log = ev.Log
	opt.Progress = func(done, total int64, path string) {
		if ev.Progress != nil {
			ev.Progress(proto.Progress{
				Stage: "проверка восстановлением", Path: path,
				Bytes: done, BytesTotal: total,
			})
		}
	}
	rep, err := restore.Drill(ctx, r, snap, opt)

	ok := err == nil && rep != nil && rep.OK()
	_ = a.Update(func(c *Config) error {
		if cr, cerr := c.Repo(s.RepoID); cerr == nil {
			cr.LastDrill = time.Now()
			cr.LastDrillOK = ok
		}
		return nil
	})
	if err != nil {
		a.notifyAbout(ctx, notify.Message{
			Level: notify.LevelError, Server: s.Name,
			Title: "проверка восстановлением не прошла",
			Body:  err.Error(),
		}, ev)
		return rep, err
	}
	if !rep.OK() {
		a.notifyAbout(ctx, notify.Message{
			Level: notify.LevelError, Server: s.Name,
			Title: "снимок непригоден к восстановлению",
			Body:  rep.Summary(),
		}, ev)
	}
	return rep, nil
}

// DrillDue сообщает, каким репозиториям пора пройти проверку.
func (a *App) DrillDue() []string {
	var due []string
	a.Read(func(c *Config) {
		days := c.UI.VerifyEveryDays
		if days <= 0 {
			return
		}
		for _, cr := range c.Repos {
			if time.Since(cr.LastDrill) > time.Duration(days)*24*time.Hour {
				due = append(due, cr.ID)
			}
		}
	})
	return due
}

// ServerForRepo находит любой сервер, пишущий в этот репозиторий:
// проверка восстановлением делается по снимку, а снимки принадлежат
// серверам.
func (a *App) ServerForRepo(repoID string) (*Server, bool) {
	var found *Server
	a.Read(func(c *Config) {
		for _, s := range c.Servers {
			if s.RepoID == repoID && !s.Last.Time.IsZero() {
				found = s
				return
			}
		}
	})
	return found, found != nil
}

// Secrets открывает доступ к защищённому хранилищу - нужен интерфейсу,
// чтобы сохранить токен git или ключ от S3.
func (a *App) Secrets() *secretstore.Store { return a.secrets }
