// Package discover составляет карту сервера.
//
// Это то, что превращает добавление сервера в один экран: агент сам находит
// сайты, их document root, версию PHP, связанные с ними базы, docker-стеки
// и конфигурации, а человеку остаётся расставить галочки. Ошибиться,
// перечисляя пути руками, тут гораздо проще, чем кажется, - и обнаружится
// ошибка в тот единственный день, когда бэкап понадобится.
package discover

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type Report struct {
	Hostname string `json:"hostname"`
	OS       string `json:"os"`
	Kernel   string `json:"kernel"`
	Arch     string `json:"arch"`
	Root     bool   `json:"root"`
	Agent    string `json:"agent"`

	Panel      *Panel    `json:"panel,omitempty"`
	WebServers []string  `json:"web_servers,omitempty"`
	Sites      []Site    `json:"sites,omitempty"`
	MySQL      *DBServer `json:"mysql,omitempty"`
	Postgres   *DBServer `json:"postgres,omitempty"`
	Docker     *Docker   `json:"docker,omitempty"`
	K8s        *K8s      `json:"k8s,omitempty"`
	Configs    []string  `json:"configs,omitempty"`
	Mounts     []Mount   `json:"mounts,omitempty"`
	Warnings   []string  `json:"warnings,omitempty"`
	Took       string    `json:"took"`
}

type Panel struct {
	Kind    string `json:"kind"` // hestia | ispmanager | plesk | cpanel
	Version string `json:"version,omitempty"`
	Path    string `json:"path"`
}

type Site struct {
	Name      string   `json:"name"`
	Root      string   `json:"root"`
	User      string   `json:"user,omitempty"`
	PHP       string   `json:"php,omitempty"`
	Aliases   []string `json:"aliases,omitempty"`
	SSL       bool     `json:"ssl,omitempty"`
	Databases []string `json:"databases,omitempty"`
	Size      int64    `json:"size"`
	Source    string   `json:"source"` // откуда узнали: hestia | nginx | apache | scan
}

type DBServer struct {
	Version   string     `json:"version,omitempty"`
	Socket    string     `json:"socket,omitempty"`
	Reachable bool       `json:"reachable"`
	Error     string     `json:"error,omitempty"`
	Databases []Database `json:"databases,omitempty"`
}

type Database struct {
	Name  string `json:"name"`
	Size  int64  `json:"size"`
	Owner string `json:"owner,omitempty"`
}

type Docker struct {
	Version    string   `json:"version,omitempty"`
	Containers []string `json:"containers,omitempty"`
	Running    int      `json:"running"`
	Volumes    []Volume `json:"volumes,omitempty"`
	Composes   []string `json:"composes,omitempty"`
}

type Volume struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// K8s - доступ к кластеру с этой машины.
type K8s struct {
	Version    string   `json:"version,omitempty"`
	Context    string   `json:"context,omitempty"`
	Reachable  bool     `json:"reachable"`
	Error      string   `json:"error,omitempty"`
	Namespaces []string `json:"namespaces,omitempty"`
	Secrets    int      `json:"secrets"`
	// ControlPlane означает, что рядом лежат сертификаты etcd, то есть
	// с этой машины можно снять слепок всего кластера.
	ControlPlane bool `json:"control_plane,omitempty"`
}

type Mount struct {
	Path  string `json:"path"`
	Total int64  `json:"total"`
	Free  int64  `json:"free"`
}

// Run собирает всё, что удаётся узнать, и никогда не возвращает ошибку
// целиком: недоступный docker или незапущенный PostgreSQL - это факт о
// сервере, а не причина не показать пользователю остальное.
func Run(ctx context.Context, agentVersion string) *Report {
	start := time.Now()
	r := &Report{Arch: runtime.GOARCH, Root: os.Geteuid() == 0, Agent: agentVersion}
	r.Hostname, _ = os.Hostname()
	r.OS = osName()
	r.Kernel = readTrim("/proc/sys/kernel/osrelease")

	r.WebServers = detectWebServers()
	r.Panel = detectPanel()
	r.Configs = existingPaths(configCandidates())
	r.Mounts = detectMounts()

	if r.Panel != nil && r.Panel.Kind == "hestia" {
		sites, dbOwners := hestiaSites(ctx, r.Panel.Path)
		r.Sites = sites
		r.MySQL = mysqlInfo(ctx, dbOwners)
	} else {
		r.Sites = nginxSites()
		r.MySQL = mysqlInfo(ctx, nil)
	}
	if len(r.Sites) == 0 {
		r.Sites = scanWebRoots()
	}
	fillSiteSizes(ctx, r.Sites)

	r.Postgres = postgresInfo(ctx)
	r.Docker = dockerInfo(ctx)
	r.K8s = k8sInfo(ctx)

	if !r.Root {
		r.Warnings = append(r.Warnings,
			"агент запущен не от root: часть файлов и баз может оказаться недоступна")
	}
	r.Took = time.Since(start).Round(time.Millisecond).String()
	return r
}

// --- Система --------------------------------------------------------------

func osName() string {
	f, err := os.Open("/etc/os-release")
	if err != nil {
		return runtime.GOOS
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "PRETTY_NAME="); ok {
			return strings.Trim(v, `"`)
		}
	}
	return runtime.GOOS
}

func readTrim(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func detectWebServers() []string {
	var out []string
	for _, c := range []struct{ bin, name string }{
		{"nginx", "nginx"}, {"apache2", "apache2"}, {"httpd", "httpd"}, {"caddy", "caddy"},
	} {
		if _, err := exec.LookPath(c.bin); err == nil {
			out = append(out, c.name)
		}
	}
	return out
}

func existingPaths(candidates []string) []string {
	var out []string
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			out = append(out, p)
		}
	}
	return out
}

func configCandidates() []string {
	return []string{
		"/etc/nginx", "/etc/apache2", "/etc/httpd", "/etc/php", "/etc/php-fpm.d",
		"/etc/mysql", "/etc/my.cnf", "/etc/postgresql", "/etc/redis",
		"/etc/letsencrypt", "/etc/systemd/system", "/etc/cron.d", "/etc/crontab",
		"/var/spool/cron", "/etc/fail2ban", "/etc/ufw",
	}
}

// --- Панели ---------------------------------------------------------------

func detectPanel() *Panel {
	if _, err := os.Stat("/usr/local/hestia"); err == nil {
		p := &Panel{Kind: "hestia", Path: "/usr/local/hestia"}
		for _, m := range hestiaVars.FindAllStringSubmatch(readFile("/usr/local/hestia/conf/hestia.conf"), -1) {
			if m[1] == "VERSION" {
				p.Version = m[2]
			}
		}
		return p
	}
	for _, c := range []struct{ path, kind string }{
		{"/usr/local/mgr5", "ispmanager"},
		{"/opt/psa", "plesk"},
		{"/usr/local/cpanel", "cpanel"},
		{"/usr/local/vesta", "vestacp"},
	} {
		if _, err := os.Stat(c.path); err == nil {
			return &Panel{Kind: c.kind, Path: c.path}
		}
	}
	return nil
}

var hestiaVars = regexp.MustCompile(`([A-Z_0-9]+)='([^']*)'`)

func readFile(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return string(b)
}

// hestiaSites читает данные панели напрямую из её файлов.
//
// Не через v-list-* намеренно: утилиты панели требуют root, работают
// заметно медленнее и на части версий меняют формат вывода. Файлы в
// data/users формата KEY='value' стабильны уже много лет.
func hestiaSites(ctx context.Context, root string) ([]Site, map[string][]string) {
	usersDir := path.Join(root, "data/users")
	entries, err := os.ReadDir(usersDir)
	if err != nil {
		return nil, nil
	}
	var sites []Site
	dbOwners := map[string][]string{}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		user := e.Name()

		// Базы пользователя: их имена в Hestia начинаются с логина,
		// что позволяет связать базу с сайтом даже без анализа конфигов.
		var userDBs []string
		for _, line := range strings.Split(readFile(path.Join(usersDir, user, "db.conf")), "\n") {
			kv := parseHestiaLine(line)
			if db := kv["DB"]; db != "" {
				userDBs = append(userDBs, db)
				dbOwners[db] = append(dbOwners[db], user)
			}
		}

		for _, line := range strings.Split(readFile(path.Join(usersDir, user, "web.conf")), "\n") {
			kv := parseHestiaLine(line)
			domain := kv["DOMAIN"]
			if domain == "" {
				continue
			}
			s := Site{
				Name:   domain,
				User:   user,
				Root:   path.Join("/home", user, "web", domain, "public_html"),
				SSL:    kv["SSL"] == "yes",
				Source: "hestia",
				PHP:    phpFromTemplate(kv["BACKEND_TEMPLATE"]),
			}
			if al := kv["ALIAS"]; al != "" {
				s.Aliases = strings.Split(al, ",")
			}
			// Связываем базы по префиксу имени: shop_main принадлежит
			// сайту shop.ru пользователя admin как admin_shop.
			for _, db := range userDBs {
				if dbLooksRelated(db, user, domain) {
					s.Databases = append(s.Databases, db)
				}
			}
			sites = append(sites, s)
		}
	}
	_ = ctx
	return sites, dbOwners
}

func parseHestiaLine(line string) map[string]string {
	if !strings.Contains(line, "='") {
		return nil
	}
	kv := map[string]string{}
	for _, m := range hestiaVars.FindAllStringSubmatch(line, -1) {
		kv[m[1]] = m[2]
	}
	return kv
}

// phpFromTemplate превращает "PHP-8_2" в "8.2".
func phpFromTemplate(t string) string {
	if t == "" {
		return ""
	}
	up := strings.ToUpper(t)
	i := strings.Index(up, "PHP-")
	if i < 0 {
		return ""
	}
	return strings.ReplaceAll(up[i+4:], "_", ".")
}

// dbLooksRelated связывает базу с сайтом по имени. Точного соответствия в
// Hestia нет, поэтому это подсказка для интерфейса, а не факт: галочки
// пользователь всё равно ставит сам.
func dbLooksRelated(db, user, domain string) bool {
	base := strings.TrimPrefix(db, user+"_")
	if base == db {
		return false
	}
	host := strings.SplitN(domain, ".", 2)[0]
	host = strings.ReplaceAll(host, "-", "")
	base = strings.ReplaceAll(base, "-", "")
	return strings.HasPrefix(base, host) || strings.HasPrefix(host, base)
}

// --- Сайты без панели -----------------------------------------------------

var (
	nginxRoot   = regexp.MustCompile(`(?m)^\s*root\s+([^;]+);`)
	nginxServer = regexp.MustCompile(`(?m)^\s*server_name\s+([^;]+);`)
)

// nginxSites разбирает конфигурации nginx настолько, насколько это
// осмысленно без полноценного парсера: находит server_name и root в
// каждом файле sites-enabled и conf.d.
func nginxSites() []Site {
	var files []string
	for _, dir := range []string{"/etc/nginx/sites-enabled", "/etc/nginx/conf.d", "/etc/nginx/vhosts"} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				files = append(files, path.Join(dir, e.Name()))
			}
		}
	}
	seen := map[string]bool{}
	var sites []Site
	for _, f := range files {
		body := readFile(f)
		roots := nginxRoot.FindAllStringSubmatch(body, -1)
		names := nginxServer.FindAllStringSubmatch(body, -1)
		if len(roots) == 0 || len(names) == 0 {
			continue
		}
		docroot := strings.TrimSpace(roots[0][1])
		for _, name := range strings.Fields(names[0][1]) {
			if name == "_" || name == "localhost" || seen[name] {
				continue
			}
			seen[name] = true
			sites = append(sites, Site{Name: name, Root: docroot, Source: "nginx"})
			break // первого имени достаточно, остальные попадут в алиасы
		}
	}
	return sites
}

// scanWebRoots - последняя попытка: каталоги в типовых местах.
func scanWebRoots() []Site {
	var sites []Site
	for _, base := range []string{"/var/www", "/srv/www", "/home"} {
		entries, err := os.ReadDir(base)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			p := path.Join(base, e.Name())
			if base == "/home" {
				p = path.Join(p, "web")
				if _, err := os.Stat(p); err != nil {
					continue
				}
			}
			sites = append(sites, Site{Name: e.Name(), Root: p, Source: "scan"})
		}
	}
	return sites
}

// fillSiteSizes считает объём каждого сайта.
//
// Через du, а не собственным обходом: du написан на C, работает в разы
// быстрее и на сайте из 300 тысяч файлов это разница между секундой и
// минутой ожидания в интерфейсе. Общий дедлайн защищает от сетевых
// монтирований, на которых du может задуматься надолго.
func fillSiteSizes(ctx context.Context, sites []Site) {
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	for i := range sites {
		if sites[i].Root == "" {
			continue
		}
		sites[i].Size = dirSize(ctx, sites[i].Root)
	}
}

func dirSize(ctx context.Context, p string) int64 {
	out, err := exec.CommandContext(ctx, "du", "-sb", "--one-file-system", p).Output()
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return 0
	}
	n, _ := strconv.ParseInt(fields[0], 10, 64)
	return n
}

// --- Базы данных ----------------------------------------------------------

func mysqlInfo(ctx context.Context, dbOwners map[string][]string) *DBServer {
	cli := ""
	for _, name := range []string{"mysql", "mariadb"} {
		if p, err := exec.LookPath(name); err == nil {
			cli = p
			break
		}
	}
	if cli == "" {
		return nil
	}
	srv := &DBServer{}

	run := func(sql string) ([]string, error) {
		c, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		out, err := exec.CommandContext(c, cli, "--batch", "--skip-column-names", "-e", sql).Output()
		if err != nil {
			return nil, err
		}
		var lines []string
		for _, l := range strings.Split(string(out), "\n") {
			if l = strings.TrimSpace(l); l != "" {
				lines = append(lines, l)
			}
		}
		return lines, nil
	}

	ver, err := run("SELECT VERSION()")
	if err != nil {
		srv.Error = "не удалось подключиться: " + shortErr(err)
		return srv
	}
	srv.Reachable = true
	if len(ver) > 0 {
		srv.Version = ver[0]
	}

	// Размеры берутся из information_schema одним запросом: перебирать
	// базы по одной на сервере с сотней сайтов слишком долго.
	rows, err := run(`SELECT table_schema, COALESCE(SUM(data_length + index_length), 0)
	                  FROM information_schema.tables
	                  WHERE table_schema NOT IN ('information_schema','performance_schema','sys','mysql')
	                  GROUP BY table_schema`)
	if err != nil {
		srv.Error = shortErr(err)
		return srv
	}
	for _, line := range rows {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		size, _ := strconv.ParseInt(f[1], 10, 64)
		d := Database{Name: f[0], Size: size}
		if owners := dbOwners[f[0]]; len(owners) > 0 {
			d.Owner = owners[0]
		}
		srv.Databases = append(srv.Databases, d)
	}
	return srv
}

func postgresInfo(ctx context.Context) *DBServer {
	if _, err := exec.LookPath("psql"); err != nil {
		return nil
	}
	srv := &DBServer{}
	c, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	sql := `SELECT datname || '|' || pg_database_size(datname)
	        FROM pg_database WHERE NOT datistemplate AND datallowconn`
	var cmd *exec.Cmd
	if os.Geteuid() == 0 {
		cmd = exec.CommandContext(c, "su", "-s", "/bin/sh", "postgres", "-c",
			"psql -At -c "+shellQuote(sql))
	} else {
		cmd = exec.CommandContext(c, "psql", "-At", "-c", sql)
	}
	out, err := cmd.Output()
	if err != nil {
		srv.Error = "не удалось подключиться: " + shortErr(err)
		return srv
	}
	srv.Reachable = true
	for _, line := range strings.Split(string(out), "\n") {
		name, sz, ok := strings.Cut(strings.TrimSpace(line), "|")
		if !ok || name == "postgres" {
			continue
		}
		size, _ := strconv.ParseInt(sz, 10, 64)
		srv.Databases = append(srv.Databases, Database{Name: name, Size: size})
	}
	return srv
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

func shortErr(err error) string {
	s := err.Error()
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}

// --- Docker ---------------------------------------------------------------

func dockerInfo(ctx context.Context) *Docker {
	if _, err := exec.LookPath("docker"); err != nil {
		return nil
	}
	c, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	d := &Docker{}

	if out, err := exec.CommandContext(c, "docker", "version", "--format", "{{.Server.Version}}").Output(); err == nil {
		d.Version = strings.TrimSpace(string(out))
	} else {
		return d // демон не отвечает - показываем сам факт наличия docker
	}

	if out, err := exec.CommandContext(c, "docker", "ps", "-a", "--format", "{{.Names}}\t{{.State}}").Output(); err == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			name, state, _ := strings.Cut(line, "\t")
			if name == "" {
				continue
			}
			d.Containers = append(d.Containers, name)
			if state == "running" {
				d.Running++
			}
		}
	}

	// docker system df -v даёт размеры томов одним вызовом; без него
	// пришлось бы обходить /var/lib/docker/volumes руками.
	if out, err := exec.CommandContext(c, "docker", "system", "df", "-v", "--format", "{{json .}}").Output(); err == nil {
		var df struct {
			Volumes []struct {
				Name string `json:"Name"`
				Size string `json:"Size"`
			} `json:"Volumes"`
		}
		if json.Unmarshal(out, &df) == nil {
			for _, v := range df.Volumes {
				d.Volumes = append(d.Volumes, Volume{Name: v.Name, Size: parseHumanSize(v.Size)})
			}
		}
	}
	if len(d.Volumes) == 0 {
		if out, err := exec.CommandContext(c, "docker", "volume", "ls", "-q").Output(); err == nil {
			for _, name := range strings.Fields(string(out)) {
				d.Volumes = append(d.Volumes, Volume{Name: name})
			}
		}
	}

	if out, err := exec.CommandContext(c, "docker", "ps", "-a", "--filter",
		"label=com.docker.compose.project", "--format",
		"{{index .Labels \"com.docker.compose.project.config_files\"}}").Output(); err == nil {
		seen := map[string]bool{}
		for _, f := range strings.Fields(string(out)) {
			for _, one := range strings.Split(f, ",") {
				if one != "" && !seen[one] {
					seen[one] = true
					d.Composes = append(d.Composes, one)
				}
			}
		}
	}
	return d
}

// k8sInfo проверяет, доступен ли отсюда кластер Kubernetes.
func k8sInfo(ctx context.Context) *K8s {
	if _, err := exec.LookPath("kubectl"); err != nil {
		return nil
	}
	c, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	k := &K8s{}

	out, err := exec.CommandContext(c, "kubectl", "version", "-o", "json").Output()
	if err != nil {
		k.Error = "кластер недоступен: " + shortErr(err)
		return k
	}
	var v struct {
		ServerVersion struct{ GitVersion string } `json:"serverVersion"`
	}
	if json.Unmarshal(out, &v) == nil {
		k.Version = v.ServerVersion.GitVersion
	}
	if k.Version == "" {
		k.Error = "kubectl есть, но сервер не отвечает"
		return k
	}
	k.Reachable = true

	if out, err := exec.CommandContext(c, "kubectl", "config", "current-context").Output(); err == nil {
		k.Context = strings.TrimSpace(string(out))
	}
	if out, err := exec.CommandContext(c, "kubectl", "get", "ns",
		"-o", "jsonpath={.items[*].metadata.name}").Output(); err == nil {
		k.Namespaces = strings.Fields(string(out))
	}
	// Счётчик секретов - то, ради чего модуль в основном и нужен:
	// их нет в GitOps-репозитории, и потерять их дороже всего.
	if out, err := exec.CommandContext(c, "kubectl", "get", "secrets", "-A",
		"-o", "jsonpath={.items[*].metadata.name}").Output(); err == nil {
		k.Secrets = len(strings.Fields(string(out)))
	}
	if _, err := os.Stat("/etc/kubernetes/pki/etcd/server.crt"); err == nil {
		k.ControlPlane = true
	}
	return k
}

var sizeRe = regexp.MustCompile(`^([\d.]+)\s*([kKmMgGtT]?)i?[bB]?$`)

func parseHumanSize(s string) int64 {
	m := sizeRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0
	}
	switch strings.ToLower(m[2]) {
	case "k":
		v *= 1 << 10
	case "m":
		v *= 1 << 20
	case "g":
		v *= 1 << 30
	case "t":
		v *= 1 << 40
	}
	return int64(v)
}
