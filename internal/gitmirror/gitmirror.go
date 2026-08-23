// Package gitmirror ведёт историю конфигураций сервера в git-репозитории.
//
// Это НЕ хранилище бэкапов. Git для бинарных данных - плохое хранилище:
// он ничего не забывает (удалить старое можно только переписав историю),
// не умеет дельты по зашифрованным данным, а репозиторий на сотню
// гигабайт становится непригоден к клонированию.
//
// Зато git даёт то, чего снимки не дают в принципе: внятный diff. Вопрос
// «что и когда поменялось в конфиге nginx за три месяца» решается одной
// командой, а не сравнением двух архивов вручную. Поэтому сюда уезжает
// только текстовое и мелкое: конфигурации, шаблоны панели, compose-файлы
// и структура баз без данных.
//
// Работает на десктопе, а не на сервере. Значит, доступы к git-серверу
// на бэкапируемых машинах не появляются вовсе: захватив сервер, добраться
// до истории конфигураций через них нельзя.
package gitmirror

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/iamtime/autobak/internal/collect"
	"github.com/iamtime/autobak/internal/repo"
	"github.com/iamtime/autobak/internal/restore"
)

type Config struct {
	Enabled bool `json:"enabled"`

	// WorkDir - локальный рабочий клон. Обязателен: даже при пустом Remote
	// история ведётся локально, и это уже полезно.
	WorkDir string `json:"work_dir"`
	// Remote - куда пушить. Пусто - только локальная история.
	Remote string `json:"remote,omitempty"`
	Branch string `json:"branch,omitempty"`
	// Prefix - подкаталог внутри репозитория. По умолчанию servers/<сервер>,
	// чтобы несколько машин жили в одном репозитории и сравнивались между
	// собой; ветка на сервер такой возможности не даёт.
	Prefix string `json:"prefix,omitempty"`

	// SSHKey - приватный ключ для git+ssh.
	SSHKey string `json:"ssh_key,omitempty"`
	// User и Token - доступ по https. Токен не хранится в конфиге в
	// открытом виде: сюда он попадает из защищённого хранилища десктопа.
	User  string `json:"user,omitempty"`
	Token string `json:"-"`

	AuthorName  string `json:"author_name,omitempty"`
	AuthorEmail string `json:"author_email,omitempty"`

	// Include - что забирать из снимка. Пусто - разумные умолчания.
	Include []string `json:"include,omitempty"`
	// AllowSecrets разрешает выгружать файлы, похожие на секреты.
	// По умолчанию выключено: .env и приватные ключи на git-сервере -
	// это утечка, которая переживёт любую последующую «уборку истории».
	AllowSecrets bool `json:"allow_secrets,omitempty"`
	// MaxFileSize - потолок для одного файла. Всё крупнее пропускается:
	// git на таком быстро становится неработоспособен.
	MaxFileSize int64 `json:"max_file_size,omitempty"`

	Log func(level, msg string) `json:"-"`
}

func (c *Config) defaults(server string) {
	if c.Branch == "" {
		c.Branch = "main"
	}
	if c.Prefix == "" {
		c.Prefix = path.Join("servers", sanitize(server))
	}
	if c.AuthorName == "" {
		c.AuthorName = "autobak"
	}
	if c.AuthorEmail == "" {
		c.AuthorEmail = "autobak@localhost"
	}
	if c.MaxFileSize == 0 {
		c.MaxFileSize = 1 << 20
	}
	if len(c.Include) == 0 {
		c.Include = DefaultInclude()
	}
}

// DefaultInclude - то, что осмысленно смотреть в diff.
func DefaultInclude() []string {
	return []string{
		"/etc",
		"/usr/local/hestia/data",
		"/usr/local/hestia/conf",
		collect.VirtualDocker + "/compose",
		collect.VirtualDocker + "/inventory.json",
		collect.VirtualK8s,
		collect.VirtualMySQL,
		collect.VirtualPostgres,
	}
}

// secretPatterns - файлы, которые не должны уехать на чужой сервер.
//
// Список намеренно избыточен: ложное срабатывание стоит одного
// непопавшего в историю файла, пропуск - утёкшего пароля от продакшна.
var secretPatterns = []string{
	".env", "*.env", "env.*", "*.key", "*.pem", "*.p12", "*.pfx",
	"id_rsa*", "id_ed25519*", "id_ecdsa*", "id_dsa*",
	"*.jks", "my.cnf", ".my.cnf", "*.my.cnf", "mysql.conf", "*credentials*", "*secret*",
	"shadow", "gshadow", "*.kdbx", "htpasswd", ".htpasswd", "*.htpasswd", "*.ovpn",
	// Файлы паролей и приложений с инлайновыми доступами к БД.
	".pgpass", ".netrc", ".pgservice.conf",
	"wp-config.php", "configuration.php", "settings.php", "local.xml",
	"parameters.yml", "secrets.yml", "database.yml", "*.pkcs12",
}

var secretDirs = []string{
	"/etc/ssl/private", "/etc/letsencrypt/archive", "/etc/letsencrypt/live",
	"/etc/ssh", "/usr/local/hestia/ssl", "/root/.ssh",
}

// looksK8sSecret ловит секреты Kubernetes. Они лежат по предсказуемым
// путям вида /@k8s/namespaces/<ns>/secret/<имя>.yaml, и внутри - пароли
// и приватные ключи в base64, то есть в открытом виде для любого, кто
// получит доступ к git-серверу.
func looksK8sSecret(p string) bool {
	if !strings.HasPrefix(p, collect.VirtualK8s+"/") {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "secret" || seg == "secrets" || seg == "etcd" {
			return true
		}
	}
	return false
}

func looksSecret(p string) bool {
	if looksK8sSecret(p) {
		return true
	}
	for _, d := range secretDirs {
		if p == d || strings.HasPrefix(p, d+"/") {
			return true
		}
	}
	name := strings.ToLower(path.Base(p))
	for _, pat := range secretPatterns {
		if ok, _ := path.Match(strings.ToLower(pat), name); ok {
			return true
		}
	}
	return false
}

type Report struct {
	Commit   string   `json:"commit,omitempty"`
	Files    int      `json:"files"`
	Skipped  int      `json:"skipped"`
	Secrets  []string `json:"secrets,omitempty"`
	Changed  bool     `json:"changed"`
	Pushed   bool     `json:"pushed"`
	Duration string   `json:"duration"`
}

func (r *Report) Summary() string {
	if !r.Changed {
		return fmt.Sprintf("конфигурации не изменились (%d файлов)", r.Files)
	}
	s := fmt.Sprintf("коммит %s: файлов %d", shortSHA(r.Commit), r.Files)
	if len(r.Secrets) > 0 {
		s += fmt.Sprintf(", секретов пропущено %d", len(r.Secrets))
	}
	if r.Pushed {
		s += ", отправлено в remote"
	}
	return s
}

func shortSHA(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// Sync выкладывает конфигурации из снимка в git и делает коммит.
func Sync(ctx context.Context, r *repo.Repo, snap *repo.Snapshot, cfg Config) (*Report, error) {
	start := time.Now()
	cfg.defaults(snap.Server)
	if cfg.WorkDir == "" {
		return nil, errors.New("autobak: не указан рабочий каталог git-зеркала")
	}
	if _, err := exec.LookPath("git"); err != nil {
		return nil, errors.New("autobak: git не найден - установите его или отключите git-зеркало")
	}

	g := &git{dir: cfg.WorkDir, cfg: &cfg}
	if err := g.ensureRepo(ctx); err != nil {
		return nil, err
	}

	// Каталог сервера очищается целиком: без этого удалённый на сервере
	// конфиг остался бы в git навсегда и diff врал бы.
	targetDir := filepath.Join(cfg.WorkDir, filepath.FromSlash(cfg.Prefix))
	if err := os.RemoveAll(targetDir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return nil, err
	}

	t := &gitTarget{dir: targetDir, cfg: &cfg}
	if _, err := restore.Run(ctx, r, snap, restore.Options{Include: cfg.Include}, t); err != nil {
		return nil, err
	}
	if err := g.writeManifest(snap, t); err != nil {
		return nil, err
	}

	rep := &Report{Files: t.files, Skipped: t.skipped, Secrets: t.secrets}
	changed, err := g.hasChanges(ctx)
	if err != nil {
		return nil, err
	}
	rep.Changed = changed
	if !changed {
		rep.Duration = time.Since(start).Round(time.Millisecond).String()
		return rep, nil
	}

	msg := fmt.Sprintf("%s: снимок %s от %s\n\n%s",
		snap.Server, snap.ID, snap.Time.Local().Format("2006-01-02 15:04"), statsLine(snap.Stats))
	sha, err := g.commit(ctx, msg)
	if err != nil {
		return nil, err
	}
	rep.Commit = sha

	if cfg.Remote != "" {
		if err := g.push(ctx); err != nil {
			// Неудачный push не отменяет коммит: история сохранена локально
			// и уедет при следующей успешной синхронизации.
			cfg.logf("warn", "не удалось отправить в remote: "+err.Error())
		} else {
			rep.Pushed = true
		}
	}
	rep.Duration = time.Since(start).Round(time.Millisecond).String()
	return rep, nil
}

func (c *Config) logf(level, msg string) {
	if c.Log != nil {
		c.Log(level, msg)
	}
}

func sanitize(s string) string {
	out := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			return r
		}
		return '-'
	}, s)
	if out == "" {
		return "server"
	}
	return out
}

// statsLine - сводка снимка для тела коммита.
func statsLine(s repo.SnapshotStats) string {
	return fmt.Sprintf("файлов %d, данных %s, новых %s",
		s.Files, repo.HumanBytes(s.BytesTotal), repo.HumanBytes(s.BytesNew))
}

func (g *git) writeManifest(snap *repo.Snapshot, t *gitTarget) error {
	var b bytes.Buffer
	fmt.Fprintf(&b, "# %s\n\n", snap.Server)
	fmt.Fprintf(&b, "Снимок: `%s`  \nВремя: %s  \nХост: %s  \nАгент: %s\n\n",
		snap.ID, snap.Time.Local().Format("2006-01-02 15:04:05"), snap.Hostname, snap.Agent)
	fmt.Fprintf(&b, "%s\n\n## Модули\n\n", statsLine(snap.Stats))
	for _, m := range snap.Modules {
		status := "ок"
		if !m.OK() {
			status = "ОШИБКА: " + m.Err
		}
		fmt.Fprintf(&b, "- **%s** - %s, файлов %d, %s (%s)\n",
			m.Name, m.Kind, m.Files, repo.HumanBytes(m.Bytes), status)
	}
	if len(t.secrets) > 0 {
		fmt.Fprintf(&b, "\n## Не выгружено (похоже на секреты)\n\n")
		for _, s := range t.secrets {
			fmt.Fprintf(&b, "- `%s`\n", s)
		}
	}
	dir := filepath.Join(g.dir, filepath.FromSlash(g.cfg.Prefix))
	return os.WriteFile(filepath.Join(dir, "README.md"), b.Bytes(), 0o644)
}
