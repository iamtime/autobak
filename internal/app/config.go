// Package app - ядро десктопного приложения: конфигурация, связь
// с серверами, запуск бэкапов и восстановления.
//
// Здесь же проходит граница доверия: ключи от репозиториев и хранилищ
// существуют только на этой стороне. Сервер получает ровно столько,
// сколько нужно для его собственного бэкапа, и ничего сверх.
package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/iamtime/autobak/internal/gitmirror"
	"github.com/iamtime/autobak/internal/notify"
	"github.com/iamtime/autobak/internal/plan"
	"github.com/iamtime/autobak/internal/repo"
	"github.com/iamtime/autobak/internal/sshx"
)

const ConfigVersion = 1

type Config struct {
	Version int           `json:"version"`
	Repos   []*Repo       `json:"repos"`
	Servers []*Server     `json:"servers"`
	UI      UISettings    `json:"ui"`
	Notify  notify.Config `json:"notify"`
}

type UISettings struct {
	// ConfirmPhrase требует набрать имя объекта перед необратимой
	// операцией. Отключается, но по умолчанию включено: «Вы уверены?»
	// нажимается не глядя, имя сервера - нет.
	ConfirmPhrase bool `json:"confirm_phrase"`
	// VerifyEveryDays - как часто проверять целостность репозитория.
	VerifyEveryDays int `json:"verify_every_days"`
}

type RepoKind string

const (
	RepoLocal RepoKind = "local"
	RepoS3    RepoKind = "s3"
)

type Repo struct {
	ID   string   `json:"id"`
	Name string   `json:"name"`
	Kind RepoKind `json:"kind"`

	// local
	Path string `json:"path,omitempty"`

	// s3. Секретный ключ и пароль репозитория лежат не здесь, а в
	// защищённом хранилище: конфигурация - обычный JSON, который человек
	// откроет, покажет на скриншоте и приложит к письму в поддержку.
	Endpoint  string `json:"endpoint,omitempty"`
	Region    string `json:"region,omitempty"`
	Bucket    string `json:"bucket,omitempty"`
	Prefix    string `json:"prefix,omitempty"`
	AccessKey string `json:"access_key,omitempty"`
	PathStyle bool   `json:"path_style,omitempty"`

	Created time.Time `json:"created"`

	// MirrorTo - идентификатор репозитория-зеркала. Правило «три копии,
	// два носителя, одна вне площадки»: одно хранилище - одна точка отказа.
	MirrorTo string `json:"mirror_to,omitempty"`
	// LastDrill - когда в последний раз проверяли восстановлением.
	LastDrill   time.Time `json:"last_drill,omitempty"`
	LastDrillOK bool      `json:"last_drill_ok,omitempty"`
}

func (r *Repo) secretKeyID() string   { return "repo/" + r.ID + "/secret_key" }
func (r *Repo) passwordKeyID() string { return "repo/" + r.ID + "/password" }

// Location - человекочитаемое «где лежит», без секретов.
func (r *Repo) Location() string {
	if r.Kind == RepoLocal {
		return r.Path
	}
	loc := r.Bucket
	if r.Prefix != "" {
		loc += "/" + r.Prefix
	}
	return "s3://" + loc + " (" + r.Endpoint + ")"
}

type Mode string

const (
	// ModePull - десктоп сам ходит на сервер и складывает данные к себе.
	// На сервере нет ни ключей, ни доступа к хранилищу.
	ModePull Mode = "pull"
	// ModePush - сервер по таймеру пишет в S3 сам. Работает без включённого
	// компьютера, но ключ репозитория и доступ к хранилищу оказываются
	// на сервере.
	ModePush Mode = "push"
)

type Server struct {
	ID   string      `json:"id"`
	Name string      `json:"name"`
	SSH  sshx.Target `json:"ssh"`

	RepoID    string           `json:"repo_id"`
	Mode      Mode             `json:"mode"`
	Plan      plan.Plan        `json:"plan"`
	Retention repo.Retention   `json:"retention"`
	Schedule  Schedule         `json:"schedule"`
	Git       gitmirror.Config `json:"git"`

	Last LastRun `json:"last"`
}

type Schedule struct {
	Enabled bool `json:"enabled"`
	// AtHour и AtMinute - ежедневный запуск. 4:00 по умолчанию: ночью
	// нагрузка на сайт минимальна, а до утра остаётся время заметить сбой.
	AtHour   int `json:"at_hour"`
	AtMinute int `json:"at_minute"`
	// EveryHours - вместо ежедневного: раз в N часов. 0 - не используется.
	EveryHours int `json:"every_hours,omitempty"`
}

// Validate отклоняет заведомо ломающие значения. Без неё отрицательное
// EveryHours превращает Next в момент из прошлого, и планировщик считает
// сервер «пора» на каждом тике - бесконечный бэкап и поток уведомлений.
func (s Schedule) Validate() error {
	if s.EveryHours < 0 {
		return fmt.Errorf("autobak: «раз в N часов» не может быть отрицательным (%d)", s.EveryHours)
	}
	if s.EveryHours > 24*7 {
		return fmt.Errorf("autobak: «раз в N часов» слишком велико (%d)", s.EveryHours)
	}
	if s.AtHour < 0 || s.AtHour > 23 {
		return fmt.Errorf("autobak: час запуска должен быть от 0 до 23 (%d)", s.AtHour)
	}
	if s.AtMinute < 0 || s.AtMinute > 59 {
		return fmt.Errorf("autobak: минута запуска должна быть от 0 до 59 (%d)", s.AtMinute)
	}
	return nil
}

func (s Schedule) Describe() string {
	if !s.Enabled {
		return "вручную"
	}
	if s.EveryHours > 0 {
		return fmt.Sprintf("каждые %d ч", s.EveryHours)
	}
	return fmt.Sprintf("ежедневно в %02d:%02d", s.AtHour, s.AtMinute)
}

// Next возвращает ближайший момент запуска после from.
func (s Schedule) Next(from, last time.Time) time.Time {
	if !s.Enabled {
		return time.Time{}
	}
	if s.EveryHours > 0 {
		if last.IsZero() {
			return from
		}
		return last.Add(time.Duration(s.EveryHours) * time.Hour)
	}
	next := time.Date(from.Year(), from.Month(), from.Day(), s.AtHour, s.AtMinute, 0, 0, from.Location())
	if !next.After(from) {
		next = next.AddDate(0, 0, 1)
	}
	return next
}

type LastRun struct {
	Time       time.Time `json:"time,omitempty"`
	SnapshotID string    `json:"snapshot_id,omitempty"`
	OK         bool      `json:"ok"`
	Error      string    `json:"error,omitempty"`
	Bytes      int64     `json:"bytes,omitempty"`
	Duration   string    `json:"duration,omitempty"`
	// Partial - снимок сделан, но часть модулей не отработала.
	// Отдельно от OK, потому что в интерфейсе это третье состояние,
	// а не «всё хорошо» и не «всё пропало».
	Partial bool `json:"partial,omitempty"`

	// Good* - последний УДАЧНЫЙ бэкап, сохраняется сквозь неудачи.
	//
	// Без этого одна неудачная попытка затирала бы ссылку на последний
	// хороший снимок: рвалась бы родословная (Parent пустел), а интерфейс
	// и проверка «давно не было» теряли бы время последнего успеха и
	// принимали бы время неудачной попытки за время бэкапа.
	GoodTime       time.Time `json:"good_time,omitempty"`
	GoodSnapshotID string    `json:"good_snapshot_id,omitempty"`
	GoodBytes      int64     `json:"good_bytes,omitempty"`
	// GoodStored - сколько прошлый успешный бэкап занял в хранилище (после
	// сжатия и дедупликации). Именно это число - ответ на «сколько места».
	GoodStored int64 `json:"good_stored,omitempty"`
}

func (l LastRun) Status() string {
	switch {
	case l.Time.IsZero():
		return "ещё не бэкапился"
	case !l.OK:
		return "ошибка"
	case l.Partial:
		return "с замечаниями"
	}
	return "в порядке"
}

func DefaultConfig() *Config {
	return &Config{
		Version: ConfigVersion,
		UI:      UISettings{ConfirmPhrase: true, VerifyEveryDays: 30},
		Notify:  notify.DefaultConfig(),
	}
}

// ConfigDir - где живут настройки и секреты.
func ConfigDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("autobak: не определить каталог настроек: %w", err)
	}
	return filepath.Join(base, "autobak"), nil
}

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return DefaultConfig(), nil
		}
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("autobak: настройки повреждены (%s): %w", path, err)
	}
	if c.Version > ConfigVersion {
		return nil, fmt.Errorf(
			"autobak: настройки от более новой версии программы (%d) - обновите autobak", c.Version)
	}
	if c.UI.VerifyEveryDays == 0 {
		c.UI.VerifyEveryDays = 30
	}
	if c.Notify.StaleAfter == 0 {
		c.Notify.StaleAfter = notify.DefaultConfig().StaleAfter
	}
	return &c, nil
}

func saveConfig(path string, c *Config) error {
	c.Version = ConfigVersion
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	// Через временный файл: оборванная запись настроек не должна
	// приводить к потере списка серверов и, что важнее, репозиториев.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (c *Config) Server(id string) (*Server, error) {
	for _, s := range c.Servers {
		if s.ID == id || strings.EqualFold(s.Name, id) {
			return s, nil
		}
	}
	return nil, fmt.Errorf("autobak: сервер %q не найден", id)
}

func (c *Config) Repo(id string) (*Repo, error) {
	for _, r := range c.Repos {
		if r.ID == id || strings.EqualFold(r.Name, id) {
			return r, nil
		}
	}
	return nil, fmt.Errorf("autobak: репозиторий %q не найден", id)
}

func (c *Config) RemoveServer(id string) error {
	i := slices.IndexFunc(c.Servers, func(s *Server) bool { return s.ID == id })
	if i < 0 {
		return fmt.Errorf("autobak: сервер %q не найден", id)
	}
	c.Servers = slices.Delete(c.Servers, i, i+1)
	return nil
}

// RemoveRepo не даёт удалить репозиторий, которым кто-то пользуется:
// иначе сервер остался бы настроенным в пустоту и молча перестал
// бэкапиться.
func (c *Config) RemoveRepo(id string) error {
	for _, s := range c.Servers {
		if s.RepoID == id {
			return fmt.Errorf(
				"autobak: репозиторий используется сервером %q - сначала переключите его", s.Name)
		}
	}
	i := slices.IndexFunc(c.Repos, func(r *Repo) bool { return r.ID == id })
	if i < 0 {
		return errors.New("autobak: репозиторий не найден")
	}
	c.Repos = slices.Delete(c.Repos, i, i+1)
	return nil
}
