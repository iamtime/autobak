// Package uiapi - ядро интерфейса, общее для окна и веба.
//
// Оба интерфейса вызывают одни и те же методы. Это не борьба за
// переиспользование ради него самого: два интерфейса, написанные
// порознь, расходятся уже через месяц, и расходятся они молча - в
// одном из них подтверждение перед необратимым действием оказывается
// слабее, чем в другом.
//
// Здесь нет ни Wails, ни HTTP: события уходят через функцию emit,
// которую подставляет тот, кто этот пакет использует.
package uiapi

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/iamtime/autobak/internal/app"
	"github.com/iamtime/autobak/internal/discover"
	"github.com/iamtime/autobak/internal/gitmirror"
	"github.com/iamtime/autobak/internal/notify"
	"github.com/iamtime/autobak/internal/plan"
	"github.com/iamtime/autobak/internal/proto"
	"github.com/iamtime/autobak/internal/repo"
	"github.com/iamtime/autobak/internal/restore"
	"github.com/iamtime/autobak/internal/sshx"
)

// Version подставляется сборкой и показывается в интерфейсе.
var Version = "dev"

// UI - единственный объект, доступный из интерфейса.
//
// Всё, что делает окно, проходит через эти методы, и они же используются
// командной строкой. Ни одной операции «только для интерфейса» нет
// намеренно: то, что нельзя выполнить командой, нельзя поставить
// в расписание и нельзя воспроизвести при разборе аварии.
type API struct {
	a *app.App
	// emitFn доставляет события интерфейсу: в окне это событие Wails,
	// в вебе - сообщение в поток SSE.
	emitFn func(name string, data any)

	mu     sync.Mutex
	cancel context.CancelFunc
	busy   string

	baseCtx context.Context
}

// New создаёт ядро. emit может быть nil, если события никому не нужны
// (например, в тестах).
func New(a *app.App, emit func(name string, data any)) *API {
	return &API{a: a, emitFn: emit, baseCtx: context.Background()}
}

// App отдаёт ядро приложения - нужно оконной сборке для диалогов ОС.
func (u *API) App() *app.App { return u.a }

// SetEmit подменяет доставку событий уже после создания: окно узнаёт
// свой контекст только при запуске.
func (u *API) SetEmit(fn func(name string, data any)) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.emitFn = fn
}

// ConfigDir - каталог с настройками и хранилищем паролей.
//
// Показывается в разделе «О программе»: человек должен знать, что именно
// копировать при переносе на другую машину и что беречь.
func (u *API) ConfigDir() string { return u.a.Dir() }

// Busy сообщает, занято ли ядро длительной операцией.
func (u *API) Busy() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.busy
}

func (u *API) emit(name string, data any) {
	if u.emitFn != nil {
		u.emitFn(name, data)
	}
}

// begin занимает приложение длительной операцией.
//
// Одновременно выполняется только одна: параллельный бэкап и prune по
// одному репозиторию - верный способ получить снимок, ссылающийся на
// только что удалённые чанки.
func (u *API) begin(what string) (context.Context, func(), error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.busy != "" {
		return nil, nil, fmt.Errorf("уже выполняется: %s", u.busy)
	}
	ctx, cancel := context.WithCancel(context.Background())
	u.cancel, u.busy = cancel, what
	u.emit("busy", what)
	return ctx, func() {
		u.mu.Lock()
		u.cancel, u.busy = nil, ""
		u.mu.Unlock()
		cancel()
		u.emit("busy", "")
	}, nil
}

// Cancel останавливает текущую операцию.
func (u *API) Cancel() {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.cancel != nil {
		u.cancel()
		u.emit("log", LogLine{"warn", "операция отменяется..."})
	}
}

// LogLine - строка журнала, уходящая в интерфейс. Экспортирована,
// чтобы веб-версия могла продублировать важное в журнал процесса:
// бэкап по расписанию идёт ночью, когда вкладку никто не держит.
type LogLine struct {
	Level string `json:"level"`
	Msg   string `json:"msg"`
}

func (u *API) events() app.Events {
	var last time.Time
	return app.Events{
		Progress: func(p proto.Progress) {
			// Ограничение частоты здесь, а не в интерфейсе: каждое событие
			// пересекает границу Go↔JS, и сто тысяч таких переходов
			// стоят дороже, чем сам бэкап.
			if time.Since(last) < 120*time.Millisecond {
				return
			}
			last = time.Now()
			u.emit("progress", p)
		},
		Log: func(level, msg string) { u.emit("log", LogLine{level, msg}) },
	}
}

// --- Состояние ------------------------------------------------------------

type ServerView struct {
	ID        string           `json:"id"`
	Name      string           `json:"name"`
	Address   string           `json:"address"`
	Mode      string           `json:"mode"`
	RepoID    string           `json:"repo_id"`
	RepoName  string           `json:"repo_name"`
	Schedule  string           `json:"schedule"`
	Retention string           `json:"retention"`
	Status    string           `json:"status"`
	LastTime  string           `json:"last_time"`
	LastError string           `json:"last_error"`
	LastBytes string           `json:"last_bytes"`
	Modules   []ModuleView     `json:"modules"`
	Sched     app.Schedule     `json:"sched"`
	Ret       repo.Retention   `json:"ret"`
	Git       gitmirror.Config `json:"git"`
	HasPlan   bool             `json:"has_plan"`
}

type ModuleView struct {
	Kind    string `json:"kind"`
	Title   string `json:"title"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
	Detail  string `json:"detail"`
}

type RepoView struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Location string `json:"location"`
	// MirrorTo - во что зеркалируется этот репозиторий.
	MirrorTo   string `json:"mirror_to,omitempty"`
	MirrorName string `json:"mirror_name,omitempty"`
	// LastDrill - когда репозиторий в последний раз доказал пригодность
	// восстановлением. Пусто означает «ни разу», и это важнее, чем
	// выглядит: репозиторий, из которого ни разу не восстанавливали,
	// не проверен ничем.
	LastDrill   string `json:"last_drill,omitempty"`
	LastDrillOK bool   `json:"last_drill_ok"`
	DrillDue    bool   `json:"drill_due"`
}

type StateView struct {
	Servers []ServerView  `json:"servers"`
	Repos   []RepoView    `json:"repos"`
	Version string        `json:"version"`
	Notify  notify.Config `json:"notify"`
	// HasTelegramToken и HasMailPass показывают, сохранён ли секрет,
	// не раскрывая его самого: интерфейсу достаточно знать, что поле
	// заполнено, чтобы не заставлять вводить пароль заново.
	HasTelegramToken bool `json:"has_telegram_token"`
	HasMailPass      bool `json:"has_mail_pass"`
	// Platform - система, на которой работает само приложение. Интерфейс
	// один на окно и на веб, а вот хранилище паролей и планировщик у
	// Windows и Linux разные, и рассказывать про schtasks на сервере с
	// Debian было бы враньём.
	Platform string `json:"platform"`
}

// State собирает всё, что показывает интерфейс.
//
// Читается под общей блокировкой: в этот момент фоновая операция
// дописывает в конфигурацию итог бэкапа. Результат - копия, поэтому
// после возврата с ним можно работать без замка.
func (u *API) State() StateView {
	// Данные, требующие обращения к хранилищу секретов и к списку
	// просроченных проверок, берутся до захвата замка: иначе чтение
	// файла с диска задерживало бы фоновые операции.
	hasTG := u.a.Secrets().Has("notify/telegram_token")
	hasMail := u.a.Secrets().Has("notify/mail_pass")
	due := map[string]bool{}
	for _, id := range u.a.DrillDue() {
		due[id] = true
	}

	var st StateView
	u.a.Read(func(cfg *app.Config) {
		st = u.buildState(cfg, hasTG, hasMail, due)
	})
	return st
}

func (u *API) buildState(cfg *app.Config, hasTG, hasMail bool, due map[string]bool) StateView {
	st := StateView{
		Version:          Version,
		Platform:         runtime.GOOS,
		Notify:           cfg.Notify,
		HasTelegramToken: hasTG,
		HasMailPass:      hasMail,
	}
	for _, r := range cfg.Repos {
		v := RepoView{
			ID: r.ID, Name: r.Name, Kind: string(r.Kind), Location: r.Location(),
			MirrorTo: r.MirrorTo, LastDrillOK: r.LastDrillOK, DrillDue: due[r.ID],
		}
		if r.MirrorTo != "" {
			if m, err := cfg.Repo(r.MirrorTo); err == nil {
				v.MirrorName = m.Name
			}
		}
		if !r.LastDrill.IsZero() {
			v.LastDrill = r.LastDrill.Local().Format("02.01.2006")
		}
		st.Repos = append(st.Repos, v)
	}
	for _, s := range cfg.Servers {
		v := ServerView{
			ID: s.ID, Name: s.Name, Address: s.SSH.Label(), Mode: string(s.Mode),
			RepoID: s.RepoID, Schedule: s.Schedule.Describe(),
			Retention: s.Retention.Describe(), Status: s.Last.Status(),
			LastError: s.Last.Error, Sched: s.Schedule, Ret: s.Retention,
			Git: s.Git, HasPlan: len(s.Plan.Enabled()) > 0,
		}
		if r, err := cfg.Repo(s.RepoID); err == nil {
			v.RepoName = r.Name
		}
		if !s.Last.Time.IsZero() {
			v.LastTime = s.Last.Time.Local().Format("02.01.2006 15:04")
			v.LastBytes = repo.HumanBytes(s.Last.Bytes)
		}
		for _, m := range s.Plan.Modules {
			v.Modules = append(v.Modules, moduleView(m))
		}
		st.Servers = append(st.Servers, v)
	}
	return st
}

func moduleView(m plan.Module) ModuleView {
	detail := strings.Join(m.Paths, ", ")
	switch {
	case len(m.Databases) > 0:
		detail = fmt.Sprintf("баз: %d - %s", len(m.Databases), strings.Join(m.Databases, ", "))
	case len(m.Volumes) > 0:
		detail = fmt.Sprintf("томов: %d", len(m.Volumes))
	}
	if len(detail) > 160 {
		detail = detail[:157] + "…"
	}
	return ModuleView{
		Kind: string(m.Kind), Title: m.Kind.Title(), Name: m.Name,
		Enabled: m.Enabled, Detail: detail,
	}
}

// --- Настройка ------------------------------------------------------------

type NewRepo struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Path      string `json:"path"`
	Endpoint  string `json:"endpoint"`
	Region    string `json:"region"`
	Bucket    string `json:"bucket"`
	Prefix    string `json:"prefix"`
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
	PathStyle bool   `json:"path_style"`
	Password  string `json:"password"`
}

// AddRepo подключает хранилище. Возвращает recovery-код, если репозиторий
// был создан заново; пустую строку - если подключён существующий.
func (u *API) AddRepo(n NewRepo) (string, error) {
	if len(n.Password) < 8 {
		return "", errors.New("пароль должен быть не короче 8 символов")
	}
	r := &app.Repo{
		Name: n.Name, Kind: app.RepoKind(n.Kind), Path: n.Path,
		Endpoint: n.Endpoint, Region: n.Region, Bucket: n.Bucket,
		Prefix: n.Prefix, AccessKey: n.AccessKey, PathStyle: n.PathStyle,
	}
	return u.a.AddRepo(context.Background(), r, n.SecretKey, n.Password)
}

type NewServer struct {
	Name   string `json:"name"`
	Alias  string `json:"alias"`
	Host   string `json:"host"`
	Port   int    `json:"port"`
	User   string `json:"user"`
	Key    string `json:"key"`
	RepoID string `json:"repo_id"`
	Mode   string `json:"mode"`
	Sudo   bool   `json:"sudo"`
}

func (u *API) AddServer(n NewServer) error {
	if n.Port == 0 {
		n.Port = 22
	}
	if n.User == "" {
		n.User = "root"
	}
	return u.a.AddServer(&app.Server{
		Name: n.Name,
		SSH: sshx.Target{
			Alias: n.Alias, Host: n.Host, Port: n.Port,
			User: n.User, KeyPath: n.Key, Sudo: n.Sudo,
		},
		RepoID:    n.RepoID,
		Mode:      app.Mode(n.Mode),
		Retention: repo.DefaultRetention(),
		Schedule:  app.Schedule{Enabled: true, AtHour: 4},
	})
}

func (u *API) RemoveServer(id string) error {
	return u.a.Update(func(c *app.Config) error { return c.RemoveServer(id) })
}

// CheckServer проверяет связь и наличие агента - первое, что нужно после
// добавления сервера.
func (u *API) CheckServer(id string) (string, error) {
	s, err := u.a.Config().Server(id)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	return s.SSH.Version(ctx)
}

// InstallAgent заливает бинарь агента на сервер.
func (u *API) InstallAgent(id, binaryPath string) (string, error) {
	s, err := u.a.Config().Server(id)
	if err != nil {
		return "", err
	}
	f, err := os.Open(binaryPath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	ctx, done, err := u.begin("установка агента")
	if err != nil {
		return "", err
	}
	defer done()
	if err := s.SSH.Install(ctx, f); err != nil {
		return "", err
	}
	return s.SSH.Version(ctx)
}

// AuthorizedKeyLine показывает, как ограничить ключ на сервере.
//
// Ограничение доступа выводится из плана сервера: ключ разрешает читать
// ровно те каталоги и базы, которые входят в бэкап. Так даже кража ключа
// не даёт доступа ни к /etc/shadow, ни к ключу репозитория.
func (u *API) AuthorizedKeyLine(id, publicKey string, backupOnly bool) (string, error) {
	s, err := u.a.Config().Server(id)
	if err != nil {
		return "", err
	}
	var allowArgs []string
	if backupOnly {
		allowArgs = plan.AllowForPlan(&s.Plan).Args()
	}
	return sshx.AuthorizedKeyLine(publicKey, s.SSH.AgentPath, backupOnly, allowArgs), nil
}

// --- Обнаружение и план ---------------------------------------------------

func (u *API) Discover(id string) (*discover.Report, error) {
	ctx, done, err := u.begin("обследование сервера")
	if err != nil {
		return nil, err
	}
	defer done()
	return u.a.Discover(ctx, id)
}

// SuggestPlan обследует сервер и предлагает план, ничего не сохраняя.
func (u *API) SuggestPlan(id string) (*plan.Plan, error) {
	rep, err := u.Discover(id)
	if err != nil {
		return nil, err
	}
	return discover.Suggest(rep), nil
}

func (u *API) SavePlan(id string, p plan.Plan) error {
	s, err := u.a.Config().Server(id)
	if err != nil {
		return err
	}
	if err := p.Validate(); err != nil {
		return err
	}
	return u.a.Update(func(*app.Config) error { s.Plan = p; return nil })
}

// ToggleModule включает или выключает один модуль плана.
func (u *API) ToggleModule(serverID string, index int, enabled bool) error {
	s, err := u.a.Config().Server(serverID)
	if err != nil {
		return err
	}
	if index < 0 || index >= len(s.Plan.Modules) {
		return errors.New("модуль не найден")
	}
	return u.a.Update(func(*app.Config) error {
		s.Plan.Modules[index].Enabled = enabled
		return nil
	})
}

func (u *API) SaveSchedule(id string, sched app.Schedule, ret repo.Retention, mode string) error {
	s, err := u.a.Config().Server(id)
	if err != nil {
		return err
	}
	if err := sched.Validate(); err != nil {
		return err
	}
	return u.a.Update(func(*app.Config) error {
		s.Schedule, s.Retention = sched, ret
		if mode != "" {
			s.Mode = app.Mode(mode)
		}
		return nil
	})
}

func (u *API) SaveGit(id string, cfg gitmirror.Config, token string) error {
	s, err := u.a.Config().Server(id)
	if err != nil {
		return err
	}
	if token != "" {
		if err := u.a.Secrets().Set("git/"+s.ID+"/token", token); err != nil {
			return err
		}
	}
	return u.a.Update(func(*app.Config) error { s.Git = cfg; return nil })
}

// --- Операции -------------------------------------------------------------

type BackupResult struct {
	SnapshotID string   `json:"snapshot_id"`
	Files      int64    `json:"files"`
	Total      string   `json:"total"`
	New        string   `json:"new"`
	Stored     string   `json:"stored"`
	Duration   string   `json:"duration"`
	Complete   bool     `json:"complete"`
	Failed     []string `json:"failed,omitempty"`
}

func (u *API) Backup(id string) (*BackupResult, error) {
	ctx, done, err := u.begin("резервное копирование")
	if err != nil {
		return nil, err
	}
	defer done()

	snap, err := u.a.Backup(ctx, id, u.events())
	if err != nil {
		return nil, err
	}
	res := &BackupResult{
		SnapshotID: snap.ID, Files: snap.Stats.Files,
		Total:    repo.HumanBytes(snap.Stats.BytesTotal),
		New:      repo.HumanBytes(snap.Stats.BytesNew),
		Stored:   repo.HumanBytes(snap.Stats.BytesStored),
		Duration: time.Duration(snap.Stats.DurationMS * int64(time.Millisecond)).Round(time.Second).String(),
		Complete: snap.Complete(),
	}
	for _, m := range snap.Failed() {
		res.Failed = append(res.Failed, m.Name+": "+m.Err)
	}
	return res, nil
}

type SnapshotView struct {
	ID       string   `json:"id"`
	Time     string   `json:"time"`
	Files    int64    `json:"files"`
	Total    string   `json:"total"`
	Stored   string   `json:"stored"`
	Complete bool     `json:"complete"`
	Modules  []string `json:"modules"`
	Failed   []string `json:"failed,omitempty"`
}

// RunSchedule выполняет всё, чьё время подошло.
//
// Тот же обход, что делает `autobak schedule run` из планировщика
// системы. Здесь он нужен для веб-версии: там нет ни планировщика
// Windows, ни привычки открыть окно - машина стоит и занимается только
// бэкапами.
//
// Идёт через тот же замок, что и ручные операции, поэтому расписание не
// начнётся поверх начатого человеком восстановления, а ход работы виден
// в журнале интерфейса, как у любой другой операции.
func (u *API) RunSchedule() error {
	ctx, done, err := u.begin("расписание")
	if err != nil {
		return err
	}
	defer done()
	return u.a.RunDue(ctx, u.events())
}

func (u *API) Snapshots(id string) ([]SnapshotView, error) {
	snaps, err := u.a.Snapshots(context.Background(), id)
	if err != nil {
		return nil, err
	}
	out := make([]SnapshotView, 0, len(snaps))
	for _, s := range snaps {
		v := SnapshotView{
			ID: s.ID, Time: s.Time.Local().Format("02.01.2006 15:04"),
			Files: s.Stats.Files, Total: repo.HumanBytes(s.Stats.BytesTotal),
			Stored: repo.HumanBytes(s.Stats.BytesStored), Complete: s.Complete(),
		}
		for _, m := range s.Modules {
			v.Modules = append(v.Modules, m.Name)
			if !m.OK() {
				v.Failed = append(v.Failed, m.Name+": "+m.Err)
			}
		}
		out = append(out, v)
	}
	return out, nil
}

type TreeEntry struct {
	Path   string `json:"path"`
	Name   string `json:"name"`
	Dir    bool   `json:"dir"`
	Size   string `json:"size"`
	Bytes  int64  `json:"bytes"`
	Module string `json:"module"`
}

// Browse показывает содержимое снимка на одном уровне вложенности.
//
// Дерево читается потоком каждый раз, а не держится в памяти: снимок
// на миллион файлов занял бы сотни мегабайт, и окно бы просто не открылось
// на слабой машине. Чанки дерева при этом лежат в кэше репозитория,
// поэтому переход между каталогами остаётся быстрым.
func (u *API) Browse(serverID, snapshotID, prefix string) ([]TreeEntry, error) {
	ctx := context.Background()
	s, err := u.a.Config().Server(serverID)
	if err != nil {
		return nil, err
	}
	r, err := u.a.OpenRepo(ctx, s.RepoID)
	if err != nil {
		return nil, err
	}
	snap, err := r.LoadSnapshot(ctx, snapshotID)
	if err != nil {
		return nil, err
	}
	if prefix == "" {
		prefix = "/"
	}
	prefix = path.Clean(prefix)

	const maxEntries = 4000
	seen := map[string]*TreeEntry{}
	truncated := false

	err = r.ReadTree(ctx, snap, func(n *repo.Node) error {
		rest, ok := underPrefix(n.Path, prefix)
		if !ok || rest == "" {
			return nil
		}
		name, _, hasMore := strings.Cut(rest, "/")
		if len(seen) >= maxEntries {
			truncated = true
			return nil
		}
		e, exists := seen[name]
		if !exists {
			e = &TreeEntry{
				Name: name, Path: path.Join(prefix, name),
				Dir: hasMore || n.Type == repo.NodeDir, Module: n.Module,
			}
			seen[name] = e
		}
		if hasMore {
			e.Dir = true
		}
		e.Bytes += n.Size
		return nil
	})
	if err != nil {
		return nil, err
	}

	out := make([]TreeEntry, 0, len(seen))
	for _, e := range seen {
		e.Size = repo.HumanBytes(e.Bytes)
		out = append(out, *e)
	}
	// Каталоги первыми, затем по имени - как в любом файловом менеджере.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Dir != out[j].Dir {
			return out[i].Dir
		}
		return out[i].Name < out[j].Name
	})
	if truncated {
		u.emit("log", LogLine{"warn",
			fmt.Sprintf("в каталоге больше %d элементов - показаны не все", maxEntries)})
	}
	return out, nil
}

func underPrefix(p, prefix string) (string, bool) {
	if prefix == "/" {
		return strings.TrimPrefix(p, "/"), strings.HasPrefix(p, "/")
	}
	if p == prefix {
		return "", true
	}
	rest, ok := strings.CutPrefix(p, prefix+"/")
	return rest, ok
}

type RestoreRequest struct {
	ServerID   string   `json:"server_id"`
	SnapshotID string   `json:"snapshot_id"`
	Include    []string `json:"include"`
	ToServer   bool     `json:"to_server"`
	LocalDir   string   `json:"local_dir"`
	Overwrite  bool     `json:"overwrite"`
	DBMode     string   `json:"db_mode"`
	DBInPlace  bool     `json:"db_in_place"`
	// Confirm - набранное человеком имя сервера. Проверяется здесь, а не
	// в интерфейсе: подтверждение, которое можно обойти правкой JS,
	// подтверждением не является.
	Confirm string `json:"confirm"`
}

func (u *API) RestorePreview(req RestoreRequest) (*restore.Report, error) {
	ctx, done, err := u.begin("подготовка восстановления")
	if err != nil {
		return nil, err
	}
	defer done()
	return u.a.Restore(ctx, req.ServerID, u.restoreOptions(req, true), u.events())
}

func (u *API) RestoreApply(req RestoreRequest) (*restore.Report, error) {
	s, err := u.a.Config().Server(req.ServerID)
	if err != nil {
		return nil, err
	}
	// Необратимое требует набранного имени. Восстановление в отдельный
	// каталог на этом компьютере ничего не разрушает и подтверждения
	// не требует - лишние подтверждения обесценивают нужные.
	if req.ToServer || req.Overwrite || req.DBInPlace {
		if u.a.Config().UI.ConfirmPhrase && req.Confirm != s.Name {
			return nil, fmt.Errorf("для подтверждения наберите имя сервера: %s", s.Name)
		}
	}
	ctx, done, err := u.begin("восстановление")
	if err != nil {
		return nil, err
	}
	defer done()
	return u.a.Restore(ctx, req.ServerID, u.restoreOptions(req, false), u.events())
}

func (u *API) restoreOptions(req RestoreRequest, dry bool) app.RestoreOptions {
	mode := restore.DBMode(req.DBMode)
	if mode == "" {
		mode = restore.DBFile
	}
	return app.RestoreOptions{
		SnapshotID: req.SnapshotID, Include: req.Include,
		ToServer: req.ToServer, LocalDir: req.LocalDir,
		Overwrite: req.Overwrite, DBMode: mode, DBInPlace: req.DBInPlace,
		DryRun: dry,
	}
}

func (u *API) Prune(serverID string, apply bool, confirm string) (*repo.PruneReport, error) {
	s, err := u.a.Config().Server(serverID)
	if err != nil {
		return nil, err
	}
	if apply && u.a.Config().UI.ConfirmPhrase && confirm != s.Name {
		return nil, fmt.Errorf("для подтверждения наберите имя сервера: %s", s.Name)
	}
	ctx, done, err := u.begin("очистка старых снимков")
	if err != nil {
		return nil, err
	}
	defer done()
	return u.a.Prune(ctx, serverID, !apply, u.events())
}

func (u *API) Verify(repoID string, sample float64) (*repo.VerifyReport, error) {
	ctx, done, err := u.begin("проверка целостности")
	if err != nil {
		return nil, err
	}
	defer done()
	return u.a.Verify(ctx, repoID, sample, u.events())
}

// --- Второе хранилище и проверка восстановлением --------------------------

// SetMirror задаёт, куда зеркалировать репозиторий. Пустая цель отключает.
func (u *API) SetMirror(repoID, targetID string) error {
	r, err := u.a.Config().Repo(repoID)
	if err != nil {
		return err
	}
	if targetID != "" {
		if targetID == repoID {
			return errors.New("зеркалировать репозиторий в самого себя нельзя")
		}
		if _, err := u.a.Config().Repo(targetID); err != nil {
			return err
		}
	}
	return u.a.Update(func(*app.Config) error { r.MirrorTo = targetID; return nil })
}

// Mirror копирует репозиторий во второе хранилище.
func (u *API) Mirror(fromID, toID string, apply, prune bool) (*repo.MirrorReport, error) {
	ctx, done, err := u.begin("копирование во второе хранилище")
	if err != nil {
		return nil, err
	}
	defer done()

	opt := repo.DefaultMirrorOptions()
	opt.DryRun = !apply
	opt.Prune = prune
	return u.a.Mirror(ctx, fromID, toID, opt, u.events())
}

// Drill восстанавливает выборку из снимка и сверяет её с записанным.
func (u *API) Drill(serverID, snapshotID string, maxMB int64) (*restore.DrillReport, error) {
	ctx, done, err := u.begin("проверка восстановлением")
	if err != nil {
		return nil, err
	}
	defer done()
	return u.a.Drill(ctx, serverID, snapshotID,
		restore.DrillOptions{MaxBytes: maxMB << 20}, u.events())
}

// --- Уведомления ----------------------------------------------------------

// SaveNotify сохраняет настройки уведомлений. Пустые секреты означают
// «оставить как было» - иначе при каждом сохранении формы пришлось бы
// заново вводить токен.
func (u *API) SaveNotify(cfg notify.Config, telegramToken, mailPass string) error {
	if telegramToken != "" {
		if err := u.a.Secrets().Set("notify/telegram_token", telegramToken); err != nil {
			return err
		}
	}
	if mailPass != "" {
		if err := u.a.Secrets().Set("notify/mail_pass", mailPass); err != nil {
			return err
		}
	}
	return u.a.Update(func(c *app.Config) error { c.Notify = cfg; return nil })
}

// TestNotify отправляет пробное сообщение.
//
// Кнопка обязательна: настройки уведомлений проверяются один раз при
// сбое, и узнать тогда, что чат-id был неверный, - значит не узнать
// вовсе. Пробное сообщение отправляется в обход правил фильтрации.
func (u *API) TestNotify() error {
	cfg := u.a.Config().Notify
	if !cfg.Configured() {
		return errors.New("не настроен ни один канал")
	}
	cfg.Enabled = true
	cfg.OnSuccess = true
	cfg.Telegram.Token, _ = u.a.Secrets().Get("notify/telegram_token")
	cfg.Mail.Pass, _ = u.a.Secrets().Get("notify/mail_pass")

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	return notify.Send(ctx, cfg, notify.Message{
		Level: notify.LevelInfo,
		Title: "проверка связи",
		Body:  "Если вы это читаете, уведомления AutoBak настроены верно.",
	})
}

// CheckStale прогоняет проверку «давно не было бэкапа» вручную.
func (u *API) CheckStale() []string {
	return u.a.CheckStale(context.Background(), u.events())
}
