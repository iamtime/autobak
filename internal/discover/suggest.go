package discover

import (
	"fmt"
	"path"

	"github.com/iamtime/autobak/internal/plan"
	"github.com/iamtime/autobak/internal/repo"
)

// Suggest превращает карту сервера в готовый план.
//
// Всё найденное попадает в план сразу включённым - кроме того, что почти
// никогда не нужно (образы docker) и что может оказаться огромным. Логика
// умолчаний простая: пропущенный по невнимательности сайт обнаружится в
// момент аварии, а лишний включённый модуль стоит лишь немного места.
func Suggest(r *Report) *plan.Plan {
	p := plan.New(r.Hostname)

	for _, s := range r.Sites {
		if s.Root == "" {
			continue
		}
		m := plan.Module{
			Kind: plan.KindFiles, Name: s.Name, Enabled: true,
			Paths: []string{s.Root}, OneFilesystem: true,
		}
		// Для сайта под панелью забираем весь его каталог, а не только
		// public_html: рядом лежат логи, конфиги домена и, что важнее,
		// каталоги вроде private и tmp, куда складывают загрузки.
		if s.Source == "hestia" && s.User != "" {
			m.Paths = []string{path.Join("/home", s.User, "web", s.Name)}
		}
		p.Modules = append(p.Modules, m)
	}

	if r.MySQL != nil && r.MySQL.Reachable && len(r.MySQL.Databases) > 0 {
		var names []string
		for _, d := range r.MySQL.Databases {
			names = append(names, d.Name)
		}
		p.Modules = append(p.Modules, plan.Module{
			Kind: plan.KindMySQL, Name: "MySQL", Enabled: true, Databases: names,
		})
	}
	if r.Postgres != nil && r.Postgres.Reachable && len(r.Postgres.Databases) > 0 {
		var names []string
		for _, d := range r.Postgres.Databases {
			names = append(names, d.Name)
		}
		p.Modules = append(p.Modules, plan.Module{
			Kind: plan.KindPostgres, Name: "PostgreSQL", Enabled: true, Databases: names,
		})
	}

	if r.Docker != nil && len(r.Docker.Volumes) > 0 {
		var vols []string
		for _, v := range r.Docker.Volumes {
			vols = append(vols, v.Name)
		}
		p.Modules = append(p.Modules, plan.Module{
			Kind: plan.KindDocker, Name: "Docker", Enabled: true,
			Volumes: vols, Compose: r.Docker.Composes,
			// Образы по умолчанию не сохраняются: они тянутся из реестра
			// одной командой, а весят больше, чем все данные вместе взятые.
			SaveImages: false,
			// Остановка контейнеров - простой сервиса, включать её должен
			// человек, понимая цену.
			StopForDump: false,
		})
	}

	if r.K8s != nil && r.K8s.Reachable {
		p.Modules = append(p.Modules, plan.Module{
			Kind: plan.KindK8s, Name: "Kubernetes: манифесты и секреты", Enabled: true,
			Context: r.K8s.Context,
			// Секреты включены, слепок etcd - нет: первое невозможно
			// восстановить ниоткуда, второе весит сотни мегабайт и нужно
			// только для возрождения самого управляющего узла.
			EtcdSnapshot: false,
		})
	}

	if len(r.Configs) > 0 {
		p.Modules = append(p.Modules, plan.Module{
			Kind: plan.KindConfigs, Name: "Конфигурации системы", Enabled: true,
			Paths: r.Configs,
		})
	}

	if r.Panel != nil && r.Panel.Kind == "hestia" {
		p.Modules = append(p.Modules, plan.Module{
			Kind: plan.KindHestia, Name: "HestiaCP: настройки и шаблоны", Enabled: true,
			Paths: plan.HestiaPaths(),
		})
	}
	return p
}

// Summary - то, что показывается человеку сразу после подключения сервера.
func (r *Report) Summary() string {
	s := r.OS
	if r.Panel != nil {
		s += " · " + r.Panel.Kind
		if r.Panel.Version != "" {
			s += " " + r.Panel.Version
		}
	}
	var total int64
	for _, x := range r.Sites {
		total += x.Size
	}
	if len(r.Sites) > 0 {
		s += fmt.Sprintf(" · сайтов: %d (%s)", len(r.Sites), repo.HumanBytes(total))
	}
	if r.MySQL != nil && r.MySQL.Reachable {
		var dbSize int64
		for _, d := range r.MySQL.Databases {
			dbSize += d.Size
		}
		s += fmt.Sprintf(" · MySQL: %d баз (%s)", len(r.MySQL.Databases), repo.HumanBytes(dbSize))
	}
	if r.Postgres != nil && r.Postgres.Reachable {
		s += fmt.Sprintf(" · PostgreSQL: %d баз", len(r.Postgres.Databases))
	}
	if r.Docker != nil && r.Docker.Version != "" {
		s += fmt.Sprintf(" · Docker: %d контейнеров, %d томов",
			len(r.Docker.Containers), len(r.Docker.Volumes))
	}
	if r.K8s != nil && r.K8s.Reachable {
		s += fmt.Sprintf(" · Kubernetes %s: %d пространств, %d секретов",
			r.K8s.Version, len(r.K8s.Namespaces), r.K8s.Secrets)
	}
	return s
}

// EstimatedSize - сколько данных придётся передать при первом бэкапе.
// Нужно, чтобы честно сказать «первый раз это займёт часы», а не делать
// вид, что всё мгновенно.
func (r *Report) EstimatedSize() int64 {
	var total int64
	for _, s := range r.Sites {
		total += s.Size
	}
	if r.MySQL != nil {
		for _, d := range r.MySQL.Databases {
			total += d.Size
		}
	}
	if r.Postgres != nil {
		for _, d := range r.Postgres.Databases {
			total += d.Size
		}
	}
	if r.Docker != nil {
		for _, v := range r.Docker.Volumes {
			total += v.Size
		}
	}
	return total
}
