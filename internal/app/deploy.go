package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/iamtime/autobak/internal/collect"
	"github.com/iamtime/autobak/internal/discover"
	"github.com/iamtime/autobak/internal/plan"
	"github.com/iamtime/autobak/internal/proto"
	"github.com/iamtime/autobak/internal/repo"
	"github.com/iamtime/autobak/internal/restore"
	"github.com/iamtime/autobak/internal/sshx"
)

// Развёртывание снимка на чистый сервер.
//
// Это не «восстановление на место», а переезд: данные одного сервера
// раскладываются на другой, обычно только что арендованный. Отсюда два
// отличия.
//
// Первое - порядок. Конфигурации раньше сайтов (иначе веб-сервер не будет
// знать, где их искать), базы раньше запуска приложений, docker в самом
// конце. Второе - честный список того, что программа сделать не может:
// переключить DNS, перевыпустить сертификаты под новый адрес, проверить
// работу. Молчание об этом создаёт впечатление законченного переезда,
// каким он не является.

type DeployOptions struct {
	// Source - сервер, чей снимок разворачиваем.
	Source string
	// SnapshotID - какой именно снимок. Пусто - последний.
	SnapshotID string

	// Target - куда. Обычно другая машина, чем Source.
	Target sshx.Target
	// AgentBinary - путь к бинарю агента для установки на цель.
	// Пусто, если агент там уже есть.
	AgentBinary string

	Configs   bool
	Sites     bool
	Databases bool
	Docker    bool

	// Force разрешает разворачивать на непустой сервер. Без него
	// программа откажется, обнаружив там чужие сайты или базы.
	Force bool
	// Confirm - набранный человеком адрес цели.
	Confirm string

	DryRun bool
}

func DefaultDeployOptions() DeployOptions {
	return DeployOptions{Configs: true, Sites: true, Databases: true, Docker: true}
}

type DeployStep struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
	Err    string `json:"err,omitempty"`
}

type DeployReport struct {
	Snapshot  string       `json:"snapshot"`
	Target    string       `json:"target"`
	Steps     []DeployStep `json:"steps"`
	Checklist []string     `json:"checklist"`
	DryRun    bool         `json:"dry_run"`
	Duration  string       `json:"duration"`
}

func (r *DeployReport) OK() bool {
	for _, s := range r.Steps {
		if !s.OK {
			return false
		}
	}
	return true
}

func (r *DeployReport) Summary() string {
	done := 0
	for _, s := range r.Steps {
		if s.OK {
			done++
		}
	}
	verb := "выполнено"
	if r.DryRun {
		verb = "будет выполнено"
	}
	return fmt.Sprintf("%s шагов: %d из %d; осталось вручную: %d",
		verb, done, len(r.Steps), len(r.Checklist))
}

func (r *DeployReport) add(name string, err error, detail string) error {
	st := DeployStep{Name: name, OK: err == nil, Detail: detail}
	if err != nil {
		st.Err = err.Error()
	}
	r.Steps = append(r.Steps, st)
	return err
}

// Deploy разворачивает снимок на указанный сервер.
func (a *App) Deploy(ctx context.Context, opt DeployOptions, ev Events) (*DeployReport, error) {
	start := time.Now()
	src, err := a.cfg.Server(opt.Source)
	if err != nil {
		return nil, err
	}
	r, err := a.OpenRepo(ctx, src.RepoID)
	if err != nil {
		return nil, err
	}

	snap, err := a.pickSnapshot(ctx, r, src.Name, opt.SnapshotID)
	if err != nil {
		return nil, err
	}
	rep := &DeployReport{
		Snapshot: snap.ID, Target: opt.Target.Label(), DryRun: opt.DryRun,
	}
	if !snap.Complete() {
		ev.log("warn", "выбран неполный снимок: часть модулей при бэкапе не отработала")
	}

	// Проверка цели идёт до всего остального: развернуть поверх живого
	// сервера - необратимо, и узнать об этом надо заранее.
	target, err := a.inspectTarget(ctx, opt, rep, ev)
	if err != nil {
		return rep, err
	}
	if !opt.DryRun {
		if opt.Confirm != opt.Target.Label() {
			return rep, fmt.Errorf(
				"для подтверждения наберите адрес цели: %s", opt.Target.Label())
		}
	}

	byKind := modulesByKind(snap)
	plan := deployPlan(opt, byKind)
	if len(plan) == 0 {
		return rep, fmt.Errorf("autobak: в снимке %s нечего разворачивать", snap.ID)
	}

	for _, step := range plan {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		ev.log("info", "разворачивается: "+step.title)
		if opt.DryRun {
			rep.add(step.title, nil, "сухой прогон")
			continue
		}
		detail, err := a.deployStep(ctx, r, snap, opt, step, ev)
		if rep.add(step.title, err, detail) != nil {
			// Дальше идти нельзя: сайты без конфигураций или базы без
			// сайтов - это не половина переезда, а сломанный сервер.
			ev.log("error", step.title+": "+err.Error())
			break
		}
	}

	rep.Checklist = checklist(opt, byKind, target, snap)
	rep.Duration = time.Since(start).Round(time.Second).String()
	return rep, nil
}

func (a *App) pickSnapshot(ctx context.Context, r *repo.Repo, server, id string) (*repo.Snapshot, error) {
	if id != "" {
		return r.LoadSnapshot(ctx, id)
	}
	all, err := r.ListSnapshots(ctx)
	if err != nil {
		return nil, err
	}
	for _, s := range all {
		if s.Server == server {
			return s, nil
		}
	}
	return nil, fmt.Errorf("autobak: у сервера %q нет ни одного снимка", server)
}

// inspectTarget ставит агента и смотрит, что на цели уже есть.
func (a *App) inspectTarget(ctx context.Context, opt DeployOptions,
	rep *DeployReport, ev Events) (*discover.Report, error) {

	if opt.AgentBinary != "" && !opt.DryRun {
		f, err := os.Open(opt.AgentBinary)
		if err != nil {
			return nil, rep.add("установка агента", err, "")
		}
		defer f.Close()
		if err := opt.Target.Install(ctx, f); err != nil {
			return nil, rep.add("установка агента", err, "")
		}
	}
	ver, err := opt.Target.Version(ctx)
	if err != nil {
		return nil, rep.add("связь с целью", err,
			"агент не отвечает; укажите путь к бинарю для установки")
	}
	rep.add("связь с целью", nil, ver)

	res, err := opt.Target.RunAgent(ctx, 3*time.Minute, "discover", "--json")
	if err != nil {
		return nil, rep.add("осмотр цели", err, "")
	}
	var t discover.Report
	if err := json.Unmarshal([]byte(res.Stdout), &t); err != nil {
		return nil, rep.add("осмотр цели", err, "")
	}

	busy := len(t.Sites)
	if t.MySQL != nil {
		busy += len(t.MySQL.Databases)
	}
	detail := fmt.Sprintf("%s, сайтов %d, баз %d", t.OS, len(t.Sites), busy-len(t.Sites))
	rep.add("осмотр цели", nil, detail)

	if busy > 0 && !opt.Force && !opt.DryRun {
		return &t, fmt.Errorf(
			"на цели уже есть данные (%s). Развёртывание перезапишет их. "+
				"Если это действительно нужно, включите принудительный режим", detail)
	}
	if busy > 0 {
		ev.log("warn", "цель не пуста: "+detail)
	}
	return &t, nil
}

type deployStepSpec struct {
	title   string
	modules []string
	dbMode  restore.DBMode
}

func modulesByKind(snap *repo.Snapshot) map[string][]string {
	out := map[string][]string{}
	for _, m := range snap.Modules {
		out[m.Kind] = append(out[m.Kind], m.Name)
	}
	return out
}

// deployPlan задаёт порядок. Конфигурации раньше сайтов, базы раньше
// приложений, docker последним - иначе стек поднимется до того, как
// появятся данные, которые он ожидает увидеть.
func deployPlan(opt DeployOptions, byKind map[string][]string) []deployStepSpec {
	var out []deployStepSpec
	add := func(want bool, title string, dbMode restore.DBMode, kinds ...string) {
		if !want {
			return
		}
		var mods []string
		for _, k := range kinds {
			mods = append(mods, byKind[k]...)
		}
		if len(mods) == 0 {
			return
		}
		out = append(out, deployStepSpec{title: title, modules: mods, dbMode: dbMode})
	}

	add(opt.Configs, "конфигурации системы и панели", restore.DBSkip,
		string(plan.KindConfigs), string(plan.KindHestia))
	add(opt.Sites, "файлы сайтов", restore.DBSkip, string(plan.KindFiles))
	add(opt.Databases, "базы данных", restore.DBRestore,
		string(plan.KindMySQL), string(plan.KindPostgres))
	add(opt.Docker, "docker: тома и compose-файлы", restore.DBSkip,
		string(plan.KindDocker))
	return out
}

func (a *App) deployStep(ctx context.Context, r *repo.Repo, snap *repo.Snapshot,
	opt DeployOptions, step deployStepSpec, ev Events) (string, error) {

	args := []string{"import", "--owner", "--overwrite", "--db=" + string(step.dbMode)}
	if step.dbMode == restore.DBRestore {
		// Сервер новый и пустой, поэтому базы восстанавливаются под своими
		// именами: копия с суффиксом здесь никому не нужна, приложения
		// ищут ровно те имена, что записаны в их конфигурациях.
		args = append(args, "--db-in-place")
	}

	cmd, err := opt.Target.Agent(ctx, args...)
	if err != nil {
		return "", err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return "", err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", err
	}
	diag := make(chan string, 1)
	go func() { diag <- readTail(stderr, 16<<10) }()

	rrep, runErr := restore.Run(ctx, r, snap, restore.Options{
		Modules: step.modules,
		Log:     ev.Log,
		Progress: func(done, total int64, path string) {
			if ev.Progress != nil {
				ev.Progress(proto.Progress{
					Stage: step.title, Path: path, Bytes: done, BytesTotal: total,
				})
			}
		},
	}, restore.NewProto(stdin))
	stdin.Close()
	waitErr := cmd.Wait()
	if msg := <-diag; msg != "" {
		ev.log("info", msg)
	}
	if runErr != nil {
		return "", runErr
	}
	if waitErr != nil {
		return "", fmt.Errorf("агент на цели: %w", waitErr)
	}
	return rrep.Summary(), nil
}

// checklist перечисляет то, что программа сделать не может.
//
// Список печатается всегда, даже когда все шаги прошли: переезд не
// заканчивается копированием данных, и молчание об оставшемся создаёт
// ложное чувство завершённости.
func checklist(opt DeployOptions, byKind map[string][]string,
	target *discover.Report, snap *repo.Snapshot) []string {

	var out []string
	if len(byKind[string(plan.KindFiles)]) > 0 {
		out = append(out,
			"Переключить DNS: домены должны указывать на адрес нового сервера. "+
				"До этого сайты будут открываться со старого.")
	}
	if len(byKind[string(plan.KindConfigs)]) > 0 {
		out = append(out,
			"Перезапустить службы: systemctl restart nginx php*-fpm mariadb")
		out = append(out,
			"Сертификаты Let's Encrypt восстановлены, но привязаны к старому адресу. "+
				"После переключения DNS обновить: certbot renew --force-renewal")
	}
	if len(byKind[string(plan.KindHestia)]) > 0 {
		out = append(out,
			"Панель восстановлена по файлам. Пересобрать конфигурации: v-rebuild-all --force")
	}
	if len(byKind[string(plan.KindDocker)]) > 0 {
		out = append(out,
			"Поднять стеки docker: compose-файлы лежат в "+
				collect.VirtualDocker+"/compose, запустить docker compose up -d в их каталогах")
	}
	if len(byKind[string(plan.KindK8s)]) > 0 {
		out = append(out,
			"Манифесты Kubernetes восстановлены файлами, но не применены. "+
				"Применять: kubectl apply -R -f "+collect.VirtualK8s+"/ - проверив, что это нужный кластер")
	}
	if target != nil && target.MySQL != nil && !target.MySQL.Reachable &&
		len(byKind[string(plan.KindMySQL)]) > 0 {
		out = append(out, "На цели не отвечает MySQL - базы восстановить не удалось. "+
			"Установите сервер баз и повторите развёртывание только для баз.")
	}
	if !snap.Complete() {
		out = append(out, "Снимок был неполон: "+failedList(snap)+
			". Эти данные на новом сервере отсутствуют.")
	}
	out = append(out,
		"Проверить результат: открыть сайты, войти в панель, убедиться, что приложения видят базы.")
	return out
}

func failedList(snap *repo.Snapshot) string {
	var parts []string
	for _, m := range snap.Failed() {
		parts = append(parts, m.Name)
	}
	return strings.Join(parts, ", ")
}
