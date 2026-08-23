// Package plan описывает, что именно бэкапить.
//
// План составляется в десктопе (обычно - галочками поверх результата
// автообнаружения) и передаётся агенту как JSON. Агент не хранит своего
// мнения о том, что важно: единственный источник правды - десктоп, иначе
// при взломе сервера злоумышленник смог бы тихо исключить каталог из
// бэкапов и ждать.
package plan

import (
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strings"
)

type Kind string

const (
	KindFiles    Kind = "files"
	KindMySQL    Kind = "mysql"
	KindPostgres Kind = "postgres"
	KindDocker   Kind = "docker"
	KindConfigs  Kind = "configs"
	KindHestia   Kind = "hestia"
	KindK8s      Kind = "k8s"
)

func (k Kind) Title() string {
	switch k {
	case KindFiles:
		return "Файлы"
	case KindMySQL:
		return "MySQL / MariaDB"
	case KindPostgres:
		return "PostgreSQL"
	case KindDocker:
		return "Docker"
	case KindConfigs:
		return "Конфигурации"
	case KindHestia:
		return "HestiaCP"
	case KindK8s:
		return "Kubernetes"
	}
	return string(k)
}

type Module struct {
	Kind    Kind   `json:"kind"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`

	// files / configs / hestia
	Paths          []string `json:"paths,omitempty"`
	Excludes       []string `json:"excludes,omitempty"`
	OneFilesystem  bool     `json:"one_filesystem,omitempty"`
	FollowSymlinks bool     `json:"follow_symlinks,omitempty"`

	// mysql / postgres. Пустой список означает «все базы, кроме служебных».
	Databases []string `json:"databases,omitempty"`
	// DSN-подобные поля. Пароль здесь не хранится: агент берёт доступ из
	// /root/.my.cnf, сокета или конфигурации панели, чтобы секрет не
	// путешествовал по сети и не оседал в конфиге десктопа.
	Socket string `json:"socket,omitempty"`
	Host   string `json:"host,omitempty"`
	Port   int    `json:"port,omitempty"`
	User   string `json:"user,omitempty"`

	// kubernetes
	Kubeconfig string   `json:"kubeconfig,omitempty"`
	Context    string   `json:"context,omitempty"`
	Namespaces []string `json:"namespaces,omitempty"` // пусто - все
	// IncludeKinds добавляет типы к списку по умолчанию (например, pods),
	// ExcludeKinds - убирает.
	IncludeKinds []string `json:"include_kinds,omitempty"`
	ExcludeKinds []string `json:"exclude_kinds,omitempty"`
	// SkipSecrets исключает секреты. По умолчанию выключено: секреты -
	// единственное, что нельзя восстановить из репозитория с манифестами,
	// потому что их туда сознательно не кладут.
	SkipSecrets bool `json:"skip_secrets,omitempty"`
	// EtcdSnapshot снимает слепок etcd целиком. Требует доступа к
	// управляющему узлу и сертификатов.
	EtcdSnapshot bool   `json:"etcd_snapshot,omitempty"`
	EtcdEndpoint string `json:"etcd_endpoint,omitempty"`

	// docker
	Volumes     []string `json:"volumes,omitempty"`
	Compose     []string `json:"compose,omitempty"`
	SaveImages  bool     `json:"save_images,omitempty"`
	StopForDump bool     `json:"stop_for_dump,omitempty"`
}

type Plan struct {
	Version int      `json:"version"`
	Server  string   `json:"server"`
	Modules []Module `json:"modules"`

	// Excludes применяются ко всем файловым модулям поверх их собственных.
	Excludes []string `json:"excludes,omitempty"`

	// Nice и IONice опускают приоритет агента. По умолчанию бэкап не должен
	// быть заметен на работающем сайте - это важнее, чем закончить быстрее.
	Nice   int `json:"nice"`
	IONice int `json:"ionice"`

	// MaxFileSize пропускает файлы больше указанного (0 - без ограничения).
	// Спасает от случайно попавшего в бэкап 300-гигабайтного core dump.
	MaxFileSize int64 `json:"max_file_size,omitempty"`

	// BandwidthKBps ограничивает скорость выгрузки. Ноль - без ограничения.
	BandwidthKBps int `json:"bandwidth_kbps,omitempty"`
}

func New(server string) *Plan {
	return &Plan{Version: 1, Server: server, Nice: 10, IONice: 7, Excludes: DefaultExcludes()}
}

// DefaultExcludes - то, что почти никогда не нужно восстанавливать, но
// занимает основную часть объёма типового сайта.
//
// node_modules и vendor восстанавливаются одной командой из lock-файла;
// кэш и логи бесполезны на новом сервере; сокеты и pid-файлы невозможно
// восстановить в принципе.
func DefaultExcludes() []string {
	return []string{
		"**/node_modules",
		"**/.git/objects",
		"**/vendor/composer",
		"**/.cache",
		"**/cache/twig",
		"**/var/cache",
		"**/storage/framework/cache",
		"**/storage/logs",
		"**/wp-content/cache",
		"**/wp-content/uploads/cache",
		"**/bitrix/cache",
		"**/bitrix/managed_cache",
		"**/bitrix/stack_cache",
		"*.log",
		"*.sock",
		"*.pid",
		"*.swp",
		"core.[0-9]*",
		"**/.npm",
		"**/.composer/cache",
		"**/tmp/sess_*",
	}
}

// ConfigPaths - конфигурации, которые стоит забирать почти всегда.
// Отсутствующие пути молча пропускаются: набор зависит от дистрибутива.
func ConfigPaths() []string {
	return []string{
		"/etc/nginx",
		"/etc/apache2",
		"/etc/httpd",
		"/etc/php",
		"/etc/php-fpm.d",
		"/etc/php-fpm.conf",
		"/etc/mysql",
		"/etc/my.cnf",
		"/etc/my.cnf.d",
		"/etc/postgresql",
		"/etc/redis",
		"/etc/letsencrypt",
		"/etc/ssl/private",
		"/etc/systemd/system",
		"/etc/cron.d",
		"/etc/crontab",
		"/var/spool/cron",
		"/etc/hosts",
		"/etc/fstab",
		"/etc/sysctl.d",
		"/etc/ufw",
		"/etc/fail2ban",
	}
}

// HestiaPaths - данные и шаблоны панели. data/ содержит описания
// пользователей, доменов и пакетов; без conf/ и templates/ восстановленная
// панель не совпадёт с исходной по настройкам веб-сервера.
func HestiaPaths() []string {
	return []string{
		"/usr/local/hestia/data",
		"/usr/local/hestia/conf",
		"/usr/local/hestia/ssl",
	}
}

func (p *Plan) Enabled() []Module {
	var out []Module
	for _, m := range p.Modules {
		if m.Enabled {
			out = append(out, m)
		}
	}
	return out
}

var ErrEmptyPlan = errors.New("autobak: в плане не включён ни один модуль")

func (p *Plan) Validate() error {
	if p.Version == 0 {
		p.Version = 1
	}
	if p.Version != 1 {
		return fmt.Errorf("autobak: план версии %d не поддерживается", p.Version)
	}
	if len(p.Enabled()) == 0 {
		return ErrEmptyPlan
	}
	for i, m := range p.Modules {
		if !m.Enabled {
			continue
		}
		if m.Name == "" {
			return fmt.Errorf("autobak: модуль %d без имени", i)
		}
		switch m.Kind {
		case KindFiles, KindConfigs, KindHestia:
			if len(m.Paths) == 0 {
				return fmt.Errorf("autobak: модуль %q не содержит ни одного пути", m.Name)
			}
			for _, raw := range m.Paths {
				// Пути в плане всегда серверные, то есть POSIX. Вторая
				// проверка нужна только для запуска на Windows (тесты,
				// восстановление в локальный каталог): на Linux обе
				// функции ведут себя одинаково.
				if !path.IsAbs(raw) && !filepath.IsAbs(raw) {
					return fmt.Errorf("autobak: путь %q должен быть абсолютным", raw)
				}
				if strings.Contains(raw, "..") {
					return fmt.Errorf("autobak: путь %q содержит переход вверх", raw)
				}
			}
		case KindMySQL, KindPostgres, KindDocker, KindK8s:
		default:
			return fmt.Errorf("autobak: неизвестный тип модуля %q", m.Kind)
		}
	}
	if p.Nice < 0 || p.Nice > 19 {
		return fmt.Errorf("autobak: nice должен быть от 0 до 19, указано %d", p.Nice)
	}
	return nil
}

// Allow ограничивает, что план вообще имеет право прочитать.
//
// Задаётся на стороне сервера, в строке authorized_keys, куда её вписывает
// администратор при установке ключа:
//
//	command="/usr/local/bin/autobak-agent serve --backup-only --allow=/home,/var/www,/etc/nginx"
//
// Смысл - закрыть дыру: план приходит от клиента в каждом запросе, и без
// серверного ограничения украденный «backup-only» ключ мог бы запросить
// выгрузку /etc/shadow или самого ключа репозитория (/etc/autobak/key) и
// получить root-эксфильтрацию всего сервера. Allow привязывает ключ к тем
// каталогам, которые и так входили в бэкап, - ни байтом больше.
//
// nil означает «без ограничений»: локальный push-режим и ручной вызов
// настраивает сам администратор, и стеснять их незачем.
type Allow struct {
	// Roots - разрешённые корни для файловых модулей (files/configs/hestia).
	// Пусто при непустом Allow означает «файловые модули запрещены вовсе».
	Roots []string
	// AllowDB разрешает модули, читающие не файловую систему напрямую:
	// mysql, postgres, docker, k8s. По умолчанию запрещены - дамп всех баз
	// украденным ключом тоже утечка.
	AllowDB bool
}

// CheckAllowed проверяет, что план не выходит за серверное ограничение.
func (p *Plan) CheckAllowed(a *Allow) error {
	if a == nil {
		return nil
	}
	for _, m := range p.Enabled() {
		switch m.Kind {
		case KindFiles, KindConfigs, KindHestia:
			for _, raw := range m.Paths {
				if !withinRoots(raw, a.Roots) {
					return fmt.Errorf("autobak: путь %q вне разрешённых этому ключу каталогов "+
						"(--allow). Ключ ограничен на сервере в authorized_keys", raw)
				}
			}
		case KindMySQL, KindPostgres, KindDocker, KindK8s:
			if !a.AllowDB {
				return fmt.Errorf("autobak: модуль %q (%s) не разрешён этому ключу: "+
					"добавьте --allow-db в authorized_keys, если он нужен", m.Name, m.Kind)
			}
		}
	}
	return nil
}

// withinRoots сообщает, лежит ли path внутри одного из корней.
//
// Оба пути приводятся к чистому виду. Сравнение по границе компонента,
// чтобы /var/www-secret не считался лежащим внутри /var/www.
func withinRoots(p string, roots []string) bool {
	cp := path.Clean(p)
	for _, root := range roots {
		cr := path.Clean(root)
		if cp == cr || strings.HasPrefix(cp, cr+"/") {
			return true
		}
	}
	return false
}

// AllowForPlan выводит серверное ограничение из самого плана: разрешаем
// ровно те корни, которые план и так собирается читать, и базы - только
// если в плане есть модуль, которому они нужны. Результат вписывается в
// authorized_keys, превращая «ключ может прочитать что угодно» в «ключ
// может прочитать ровно то, что бэкапится».
func AllowForPlan(p *Plan) *Allow {
	a := &Allow{}
	seen := map[string]bool{}
	for _, m := range p.Enabled() {
		switch m.Kind {
		case KindFiles, KindConfigs, KindHestia:
			for _, raw := range m.Paths {
				r := path.Clean(raw)
				if r != "" && r != "." && !seen[r] {
					seen[r] = true
					a.Roots = append(a.Roots, r)
				}
			}
		case KindMySQL, KindPostgres, KindDocker, KindK8s:
			a.AllowDB = true
		}
	}
	return a
}

// Args превращает ограничение в аргументы для строки serve в
// authorized_keys. Пусто, если ограничивать нечего.
func (a *Allow) Args() []string {
	if a == nil {
		return nil
	}
	var out []string
	if len(a.Roots) > 0 {
		out = append(out, "--allow="+strings.Join(a.Roots, ","))
	}
	if a.AllowDB {
		out = append(out, "--allow-db")
	}
	return out
}

// ParseAllow разбирает серверное ограничение из аргументов authorized_keys.
//
// Формат: --allow=/a,/b,/c и флаг --allow-db. Возвращает nil, если ни того,
// ни другого нет, - это режим «без ограничений».
func ParseAllow(args []string) *Allow {
	var a *Allow
	ensure := func() *Allow {
		if a == nil {
			a = &Allow{}
		}
		return a
	}
	for _, arg := range args {
		switch {
		case arg == "--allow-db":
			ensure().AllowDB = true
		case strings.HasPrefix(arg, "--allow="):
			roots := strings.Split(strings.TrimPrefix(arg, "--allow="), ",")
			for _, r := range roots {
				if r = strings.TrimSpace(r); r != "" {
					ensure().Roots = append(a.Roots, path.Clean(r))
				}
			}
		}
	}
	return a
}

// Describe - короткое человеческое описание плана для подтверждений.
func (p *Plan) Describe() string {
	var parts []string
	for _, m := range p.Enabled() {
		parts = append(parts, m.Kind.Title()+": "+m.Name)
	}
	if len(parts) == 0 {
		return "пустой план"
	}
	return strings.Join(parts, "; ")
}
