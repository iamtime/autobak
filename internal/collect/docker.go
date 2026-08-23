package collect

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"slices"
	"strings"

	"github.com/iamtime/autobak/internal/plan"
)

// dockerVolumeRoot - где docker с драйвером local держит данные томов.
// Читать их напрямую быстрее и надёжнее, чем поднимать вспомогательный
// контейнер с tar: не нужен ни один образ, и ничего не ломается, если
// на сервере нет интернета.
const dockerVolumeRoot = "/var/lib/docker/volumes"

type dockerCollector struct {
	m plan.Module

	volumes    []string
	containers []dockerContainer
	stopped    []string
}

type dockerContainer struct {
	ID      string            `json:"Id"`
	Name    string            `json:"Name"`
	Image   string            `json:"Image"`
	State   string            `json:"State"`
	Labels  map[string]string `json:"Labels"`
	Volumes []string          `json:"Volumes"`
}

func newDocker(m plan.Module) *dockerCollector { return &dockerCollector{m: m} }

func (c *dockerCollector) Kind() plan.Kind { return plan.KindDocker }
func (c *dockerCollector) Name() string    { return c.m.Name }

func (c *dockerCollector) Collect(ctx context.Context, s Sink) (map[string]any, error) {
	if _, err := lookPath("docker", "docker не установлен или недоступен агенту"); err != nil {
		return nil, err
	}

	if err := c.loadContainers(ctx, s); err != nil {
		return nil, err
	}

	vols := c.m.Volumes
	if len(vols) == 0 {
		var err error
		if vols, err = c.listVolumes(ctx); err != nil {
			return nil, err
		}
	}
	c.volumes = vols

	// Описание стека сохраняется всегда и первым: без него том - это просто
	// каталог с данными, из которого непонятно, что и как запускать.
	if err := c.dumpInventory(ctx, s); err != nil {
		s.Logf("warn", "не удалось сохранить описание контейнеров: %v", err)
	}
	if err := c.dumpComposeFiles(ctx, s); err != nil {
		s.Logf("warn", "не удалось сохранить compose-файлы: %v", err)
	}

	if c.m.StopForDump {
		// Остановка даёт консистентный снимок баз, живущих в томах: сбросить
		// на диск незавершённые записи иначе нечем. Это простой сервиса,
		// поэтому включается только явно.
		c.stopContainers(ctx, s)
		defer c.startContainers(ctx, s)
	} else if len(vols) > 0 && c.hasRunning(ctx) {
		// Без остановки том копируется у работающего контейнера. Для баз,
		// живущих в томе (postgres/mysql в docker), это чревато торн-файлами
		// данных: половина записи попадёт в снимок, и база не поднимется.
		// Молча делать вид, что всё хорошо, нельзя - предупреждаем.
		s.Logf("warn", "тома копируются у работающих контейнеров без остановки: "+
			"снимок базы, живущей в томе, может оказаться несогласованным. "+
			"Для баз в docker включите «останавливать на время дампа» или бэкапьте их модулем mysql/postgres")
	}

	var failed []string
	for _, v := range vols {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := c.dumpVolume(ctx, s, v); err != nil {
			failed = append(failed, v)
			s.Logf("error", "том %s не сохранён: %v", v, err)
		}
	}

	if c.m.SaveImages {
		if err := c.dumpImages(ctx, s); err != nil {
			s.Logf("warn", "не удалось выгрузить образы: %v", err)
		}
	}

	meta := map[string]any{
		"volumes":     vols,
		"containers":  len(c.containers),
		"save_images": c.m.SaveImages,
	}
	if len(failed) > 0 {
		meta["failed"] = failed
		return meta, fmt.Errorf("autobak: не сохранены тома: %s", strings.Join(failed, ", "))
	}
	return meta, nil
}

// dumpVolume отдаёт содержимое тома обычными файловыми узлами, а не одним
// tar-архивом.
//
// Это принципиально для инкрементальности: изменившийся на килобайт файл
// внутри тома добавит к репозиторию килобайты, тогда как перепакованный
// tar изменился бы целиком и стоил бы как полная копия тома.
func (c *dockerCollector) dumpVolume(ctx context.Context, s Sink, name string) error {
	// Реальный путь спрашиваем у самого движка, а не угадываем: у podman
	// (podman-docker), у rootless-docker и при нестандартном data-root тома
	// лежат не в /var/lib/docker/volumes. Mountpoint из inspect верен всегда.
	src := c.volumeMountpoint(ctx, name)
	if src == "" {
		src = path.Join(dockerVolumeRoot, name, "_data") // запасной вариант
	}
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("том недоступен по пути %s: %w "+
			"(проверьте, что агент видит каталог томов движка; для rootless podman/docker "+
			"он в домашнем каталоге пользователя, а не в /var/lib)", src, err)
	}
	sub := plan.Module{
		Kind:          plan.KindFiles,
		Name:          c.m.Name,
		Paths:         []string{src},
		Excludes:      c.m.Excludes,
		OneFilesystem: true,
	}
	fc := newFiles(sub, nil)
	fc.strip = src
	fc.prefix = path.Join(VirtualDocker, "volumes", name)
	_, err := fc.Collect(ctx, s)
	return err
}

// volumeMountpoint спрашивает у движка, где физически лежат данные тома.
// Пусто, если движок недоступен или не сообщил путь - тогда используется
// запасной путь по умолчанию.
func (c *dockerCollector) volumeMountpoint(ctx context.Context, name string) string {
	if strings.HasPrefix(name, "-") {
		return "" // имя-как-флаг не отдаём в inspect
	}
	out, err := runCapture(ctx, "docker", "volume", "inspect", "--format", "{{.Mountpoint}}", name)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func (c *dockerCollector) loadContainers(ctx context.Context, s Sink) error {
	out, err := runCapture(ctx, "docker", "ps", "-a", "--no-trunc", "--format", "{{.ID}}")
	if err != nil {
		return fmt.Errorf("autobak: docker недоступен: %w", err)
	}
	ids := trimLines(out)
	if len(ids) == 0 {
		return nil
	}
	raw, err := runCapture(ctx, "docker", append([]string{"inspect"}, ids...)...)
	if err != nil {
		return err
	}
	var inspected []struct {
		ID     string `json:"Id"`
		Name   string `json:"Name"`
		State  struct{ Status string }
		Config struct {
			Image  string
			Labels map[string]string
		}
		Mounts []struct {
			Type, Name string
		}
	}
	if err := json.Unmarshal(raw, &inspected); err != nil {
		return fmt.Errorf("autobak: не разобрать вывод docker inspect: %w", err)
	}
	for _, it := range inspected {
		dc := dockerContainer{
			ID: it.ID, Name: strings.TrimPrefix(it.Name, "/"),
			Image: it.Config.Image, State: it.State.Status, Labels: it.Config.Labels,
		}
		for _, m := range it.Mounts {
			if m.Type == "volume" && m.Name != "" {
				dc.Volumes = append(dc.Volumes, m.Name)
			}
		}
		c.containers = append(c.containers, dc)
	}
	s.Logf("info", "найдено контейнеров: %d", len(c.containers))
	return nil
}

func (c *dockerCollector) listVolumes(ctx context.Context) ([]string, error) {
	out, err := runCapture(ctx, "docker", "volume", "ls", "-q")
	if err != nil {
		return nil, err
	}
	return trimLines(out), nil
}

// dumpInventory сохраняет полное описание контейнеров, сетей и образов.
func (c *dockerCollector) dumpInventory(ctx context.Context, s Sink) error {
	inv := map[string]any{"containers": c.containers}
	if out, err := runCapture(ctx, "docker", "image", "ls", "--digests", "--format", "{{json .}}"); err == nil {
		inv["images"] = trimLines(out)
	}
	if out, err := runCapture(ctx, "docker", "network", "ls", "--format", "{{json .}}"); err == nil {
		inv["networks"] = trimLines(out)
	}
	raw, err := json.MarshalIndent(inv, "", "  ")
	if err != nil {
		return err
	}
	n := virtualNode(path.Join(VirtualDocker, "inventory.json"), c.m.Name)
	return s.File(n, strings.NewReader(string(raw)))
}

// dumpComposeFiles забирает docker-compose.yml и .env каждого стека.
//
// Пути к ним docker хранит сам, в метке com.docker.compose.project.config_files,
// поэтому искать их по диску не нужно.
func (c *dockerCollector) dumpComposeFiles(ctx context.Context, s Sink) error {
	seen := map[string]bool{}
	var files []string
	for _, ct := range c.containers {
		for _, f := range strings.Split(ct.Labels["com.docker.compose.project.config_files"], ",") {
			if f = strings.TrimSpace(f); f != "" && !seen[f] {
				seen[f] = true
				files = append(files, f)
			}
		}
	}
	files = append(files, c.m.Compose...)
	slices.Sort(files)
	files = slices.Compact(files)
	if len(files) == 0 {
		return nil
	}

	// Рядом с compose-файлом почти всегда лежит .env - без него стек не
	// поднимется, а в самом compose его нет.
	var withEnv []string
	for _, f := range files {
		withEnv = append(withEnv, f)
		env := path.Join(path.Dir(f), ".env")
		if _, err := os.Stat(env); err == nil && !seen[env] {
			seen[env] = true
			withEnv = append(withEnv, env)
		}
	}

	sub := plan.Module{Kind: plan.KindFiles, Name: c.m.Name, Paths: withEnv}
	fc := newFiles(sub, nil)
	fc.strip = "/"
	fc.prefix = path.Join(VirtualDocker, "compose")
	_, err := fc.Collect(ctx, s)
	return err
}

// dumpImages выгружает образы целиком.
//
// По умолчанию выключено: образ из публичного реестра восстанавливается
// одной командой pull, а его слои весят сотни мегабайт. Включать имеет
// смысл для собственных образов, собранных локально и нигде не хранящихся.
func (c *dockerCollector) dumpImages(ctx context.Context, s Sink) error {
	used := map[string]bool{}
	for _, ct := range c.containers {
		if ct.Image != "" && !strings.HasPrefix(ct.Image, "sha256:") {
			used[ct.Image] = true
		}
	}
	for img := range used {
		n := virtualNode(path.Join(VirtualDocker, "images", safeFileName(img)+".tar"), c.m.Name)
		cmd := exec.CommandContext(ctx, "docker", "save", img)
		if err := streamCommand(ctx, s, n, cmd); err != nil {
			s.Logf("warn", "образ %s не сохранён: %v", img, err)
		}
	}
	return nil
}

// hasRunning сообщает, есть ли хоть один работающий контейнер.
func (c *dockerCollector) hasRunning(ctx context.Context) bool {
	for _, ct := range c.containers {
		if ct.State == "running" {
			return true
		}
	}
	return false
}

func (c *dockerCollector) stopContainers(ctx context.Context, s Sink) {
	for _, ct := range c.containers {
		if ct.State != "running" {
			continue
		}
		if _, err := runCapture(ctx, "docker", "stop", "-t", "30", ct.ID); err != nil {
			s.Logf("warn", "не остановлен контейнер %s: %v", ct.Name, err)
			continue
		}
		c.stopped = append(c.stopped, ct.ID)
		s.Logf("info", "контейнер %s остановлен на время бэкапа", ct.Name)
	}
}

// startContainers поднимает всё, что мы остановили.
//
// Вызывается через defer и использует собственный контекст: если бэкап
// прервали по Ctrl+C, контейнеры всё равно обязаны вернуться в строй.
func (c *dockerCollector) startContainers(_ context.Context, s Sink) {
	ctx := context.Background()
	// В обратном порядке: зависимые контейнеры останавливались последними.
	for i := len(c.stopped) - 1; i >= 0; i-- {
		if _, err := runCapture(ctx, "docker", "start", c.stopped[i]); err != nil {
			s.Logf("error", "КОНТЕЙНЕР НЕ ЗАПУЩЕН ОБРАТНО: %s: %v", c.stopped[i], err)
		}
	}
	c.stopped = nil
}

func safeFileName(s string) string {
	r := strings.NewReplacer("/", "_", ":", "_", "@", "_", " ", "_")
	return r.Replace(s)
}
