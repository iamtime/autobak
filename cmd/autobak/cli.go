package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/iamtime/autobak/internal/app"
	"github.com/iamtime/autobak/internal/discover"
	"github.com/iamtime/autobak/internal/plan"
	"github.com/iamtime/autobak/internal/proto"
	"github.com/iamtime/autobak/internal/repo"
	"github.com/iamtime/autobak/internal/restore"
	"github.com/iamtime/autobak/internal/sshx"
)

func tw() *tabwriter.Writer {
	return tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
}

// hoistFlags переносит ведущие позиционные аргументы в конец, чтобы флаги
// парсились независимо от порядка.
//
// Стандартный flag в Go останавливается на первом же аргументе без "-",
// поэтому «server install prod --binary agent» оставлял бы --binary
// неразобранным. А именно такой порядок (сначала имя сервера, потом флаги)
// естественнее всего и записан в документации. Собираем токены до первого
// флага - это заведомо позиционные аргументы, не значения флагов, - и
// ставим их после флагов; их взаимный порядок сохраняется.
func hoistFlags(args []string) []string {
	i := 0
	for i < len(args) && !strings.HasPrefix(args[i], "-") {
		i++
	}
	if i == 0 || i == len(args) {
		return args // нечего переносить: либо начинается с флага, либо флагов нет
	}
	out := make([]string, 0, len(args))
	out = append(out, args[i:]...) // флаги и их значения
	out = append(out, args[:i]...) // ведущие позиционные - в конец
	return out
}

// events выводит ход длинной операции одной перерисовываемой строкой,
// чтобы терминал не заполнялся сотней тысяч имён файлов.
func events() app.Events {
	var last time.Time
	return app.Events{
		Progress: func(p proto.Progress) {
			if time.Since(last) < 200*time.Millisecond {
				return
			}
			last = time.Now()
			path := p.Path
			if len(path) > 48 {
				path = "..." + path[len(path)-45:]
			}
			fmt.Printf("\r\033[K  %-28s %6d файлов  %10s  %s",
				trunc(p.Stage, 28), p.Files, repo.HumanBytes(p.Bytes), path)
		},
		Log: func(level, msg string) {
			if level == "info" {
				return // в терминале интересны только предупреждения и ошибки
			}
			fmt.Printf("\r\033[K[%s] %s\n", level, msg)
		},
	}
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func clearLine() { fmt.Print("\r\033[K") }

func prompt(question string) string {
	fmt.Print(question)
	sc := bufio.NewScanner(os.Stdin)
	if !sc.Scan() {
		return ""
	}
	return strings.TrimSpace(sc.Text())
}

// confirmName требует набрать имя объекта.
//
// Не «да/нет»: диалог с кнопкой «Да» пролистывается на автомате, а имя
// сервера приходится прочитать и осознанно набрать. Для необратимых
// операций это единственная защита, которая действительно работает.
func confirmName(kind, name string) error {
	fmt.Printf("\nЭто необратимо. Наберите имя %s для подтверждения (%s): ", kind, name)
	if prompt("") != name {
		return errors.New("отменено: имя не совпало")
	}
	return nil
}

// --- repo -----------------------------------------------------------------

func cmdRepo(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("укажите: repo add | repo list")
	}
	a, err := openApp()
	if err != nil {
		return err
	}
	switch args[0] {
	case "list":
		w := tw()
		fmt.Fprintln(w, "ИМЯ\tТИП\tГДЕ\tСОЗДАН")
		for _, r := range a.Config().Repos {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
				r.Name, r.Kind, r.Location(), r.Created.Local().Format("2006-01-02"))
		}
		return w.Flush()

	case "add":
		return repoAdd(ctx, a, args[1:])
	}
	return fmt.Errorf("неизвестная подкоманда %q", args[0])
}

func repoAdd(ctx context.Context, a *app.App, args []string) error {
	fs := flag.NewFlagSet("repo add", flag.ContinueOnError)
	name := fs.String("name", "", "название хранилища")
	kind := fs.String("kind", "local", "local или s3")
	path := fs.String("path", "", "каталог (для local)")
	endpoint := fs.String("endpoint", "", "адрес S3")
	region := fs.String("region", "", "регион S3")
	bucket := fs.String("bucket", "", "бакет")
	prefix := fs.String("prefix", "", "подкаталог в бакете")
	access := fs.String("access-key", "", "ключ доступа S3")
	pathStyle := fs.Bool("path-style", true, "адресация вида endpoint/bucket/key")
	if err := fs.Parse(hoistFlags(args)); err != nil {
		return err
	}
	if *name == "" {
		return errors.New("укажите --name")
	}

	r := &app.Repo{
		Name: *name, Kind: app.RepoKind(*kind), Path: *path,
		Endpoint: *endpoint, Region: *region, Bucket: *bucket, Prefix: *prefix,
		AccessKey: *access, PathStyle: *pathStyle,
	}

	var secret string
	if r.Kind == app.RepoS3 {
		secret = prompt("Секретный ключ S3: ")
		if secret == "" {
			return errors.New("без секретного ключа подключиться нельзя")
		}
	}
	password := prompt("Пароль репозитория (им шифруются данные): ")
	if len(password) < 8 {
		return errors.New("пароль короче 8 символов - это не пароль")
	}
	if prompt("Повторите пароль: ") != password {
		return errors.New("пароли не совпали")
	}

	recovery, err := a.AddRepo(ctx, r, secret, password)
	if err != nil {
		return err
	}
	if recovery == "" {
		fmt.Printf("Подключён существующий репозиторий %q.\n", r.Name)
		return nil
	}

	fmt.Printf(`
Репозиторий %q создан.

  RECOVERY-КОД. Запишите его на бумагу и уберите отдельно от компьютера.

  %s

Это единственный способ добраться до бэкапов, если вы потеряете и этот
компьютер, и пароль. Второй раз код показан не будет.
`, r.Name, recovery)
	prompt("\nНаберите «записал» и нажмите Enter: ")
	return nil
}

// --- server ---------------------------------------------------------------

func cmdServer(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("укажите: server add | list | discover | plan | install")
	}
	a, err := openApp()
	if err != nil {
		return err
	}
	switch args[0] {
	case "list":
		return serverList(a)
	case "add":
		return serverAdd(a, args[1:])
	case "discover":
		return serverDiscover(ctx, a, args[1:])
	case "plan":
		return serverPlan(ctx, a, args[1:])
	case "install":
		return serverInstall(ctx, a, args[1:])
	}
	return fmt.Errorf("неизвестная подкоманда %q", args[0])
}

func serverList(a *app.App) error {
	w := tw()
	fmt.Fprintln(w, "ИМЯ\tАДРЕС\tРЕЖИМ\tРАСПИСАНИЕ\tПОСЛЕДНИЙ БЭКАП\tСОСТОЯНИЕ")
	for _, s := range a.Config().Servers {
		last := "-"
		if !s.Last.Time.IsZero() {
			last = s.Last.Time.Local().Format("02.01 15:04")
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			s.Name, s.SSH.Label(), s.Mode, s.Schedule.Describe(), last, s.Last.Status())
	}
	return w.Flush()
}

func serverAdd(a *app.App, args []string) error {
	fs := flag.NewFlagSet("server add", flag.ContinueOnError)
	name := fs.String("name", "", "имя сервера")
	host := fs.String("host", "", "адрес")
	alias := fs.String("alias", "", "запись из ~/.ssh/config (вместо host)")
	port := fs.Int("port", 22, "порт")
	user := fs.String("user", "root", "пользователь")
	key := fs.String("key", "", "путь к приватному ключу")
	repoName := fs.String("repo", "", "хранилище")
	mode := fs.String("mode", "pull", "pull или push")
	sudo := fs.Bool("sudo", false, "запускать агента через sudo")
	if err := fs.Parse(hoistFlags(args)); err != nil {
		return err
	}
	if *name == "" || (*host == "" && *alias == "") {
		return errors.New("укажите --name и --host (или --alias)")
	}
	cr, err := a.Config().Repo(*repoName)
	if err != nil {
		return err
	}
	s := &app.Server{
		Name: *name,
		SSH: sshx.Target{
			Alias: *alias, Host: *host, Port: *port, User: *user,
			KeyPath: *key, Sudo: *sudo,
		},
		RepoID:    cr.ID,
		Mode:      app.Mode(*mode),
		Retention: repo.DefaultRetention(),
		Schedule:  app.Schedule{Enabled: true, AtHour: 4},
	}
	if err := a.AddServer(s); err != nil {
		return err
	}
	fmt.Printf("Сервер %q добавлен. Дальше: autobak server install %s\n", s.Name, s.Name)
	return nil
}

func serverInstall(ctx context.Context, a *app.App, args []string) error {
	fs := flag.NewFlagSet("server install", flag.ContinueOnError)
	binary := fs.String("binary", "", "путь к собранному autobak-agent")
	if err := fs.Parse(hoistFlags(args)); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("укажите имя сервера")
	}
	s, err := a.Config().Server(fs.Arg(0))
	if err != nil {
		return err
	}
	if *binary == "" {
		return errors.New("укажите --binary с путём к autobak-agent для Linux")
	}
	f, err := os.Open(*binary)
	if err != nil {
		return err
	}
	defer f.Close()

	fmt.Printf("Устанавливаю агента на %s...\n", s.SSH.Label())
	if err := s.SSH.Install(ctx, f); err != nil {
		return err
	}
	ver, err := s.SSH.Version(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("Установлено: %s\n\n", ver)
	fmt.Printf(`Теперь ограничьте ключ на сервере. В ~/.ssh/authorized_keys
замените строку своего ключа autobak на:

  %s

Так ключ сможет только делать бэкапы. Восстановление им запрещено
намеренно: команда восстановления пишет произвольные файлы от root,
поэтому ключ с её разрешением равносилен root.

Когда понадобится восстановить, временно уберите из строки
--backup-only:

  %s
`, sshx.AuthorizedKeyLine("<ваш-публичный-ключ>", s.SSH.AgentPath, true, plan.AllowForPlan(&s.Plan).Args()),
		sshx.AuthorizedKeyLine("<ваш-публичный-ключ>", s.SSH.AgentPath, false, nil))
	return nil
}

func serverDiscover(ctx context.Context, a *app.App, args []string) error {
	fs := flag.NewFlagSet("server discover", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "вывести полный отчёт в JSON")
	if err := fs.Parse(hoistFlags(args)); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("укажите имя сервера")
	}
	rep, err := a.Discover(ctx, fs.Arg(0))
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	printReport(rep)
	return nil
}

func printReport(rep *discover.Report) {
	fmt.Println(rep.Summary())
	fmt.Println()
	if len(rep.Sites) > 0 {
		w := tw()
		fmt.Fprintln(w, "САЙТ\tКОРЕНЬ\tPHP\tРАЗМЕР\tБАЗЫ")
		for _, s := range rep.Sites {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
				s.Name, s.Root, s.PHP, repo.HumanBytes(s.Size), strings.Join(s.Databases, ", "))
		}
		w.Flush()
		fmt.Println()
	}
	if rep.MySQL != nil && len(rep.MySQL.Databases) > 0 {
		w := tw()
		fmt.Fprintln(w, "БАЗА MySQL\tРАЗМЕР\tВЛАДЕЛЕЦ")
		for _, d := range rep.MySQL.Databases {
			fmt.Fprintf(w, "%s\t%s\t%s\n", d.Name, repo.HumanBytes(d.Size), d.Owner)
		}
		w.Flush()
		fmt.Println()
	}
	if rep.Docker != nil && len(rep.Docker.Volumes) > 0 {
		fmt.Printf("Docker %s: контейнеров %d (запущено %d), томов %d\n",
			rep.Docker.Version, len(rep.Docker.Containers), rep.Docker.Running, len(rep.Docker.Volumes))
	}
	if len(rep.Configs) > 0 {
		fmt.Printf("Конфигурации: %s\n", strings.Join(rep.Configs, ", "))
	}
	for _, warn := range rep.Warnings {
		fmt.Printf("! %s\n", warn)
	}
	fmt.Printf("\nПервый бэкап передаст примерно %s.\n", repo.HumanBytes(rep.EstimatedSize()))
}

// serverPlan составляет план по результатам обнаружения и сохраняет его.
func serverPlan(ctx context.Context, a *app.App, args []string) error {
	fs := flag.NewFlagSet("server plan", flag.ContinueOnError)
	apply := fs.Bool("apply", false, "сохранить составленный план")
	if err := fs.Parse(hoistFlags(args)); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("укажите имя сервера")
	}
	s, err := a.Config().Server(fs.Arg(0))
	if err != nil {
		return err
	}
	rep, err := a.Discover(ctx, s.ID)
	if err != nil {
		return err
	}
	p := discover.Suggest(rep)
	// Сохраняем ручные правки прошлого плана (исключения, выбор баз,
	// снятые галочки) при пересборке.
	p.CarryOver(s.Plan)

	w := tw()
	fmt.Fprintln(w, "ВКЛ\tТИП\tЧТО\tПОДРОБНОСТИ")
	for _, m := range p.Modules {
		mark := " "
		if m.Enabled {
			mark = "✓"
		}
		detail := strings.Join(m.Paths, ", ")
		if len(m.Databases) > 0 {
			detail = fmt.Sprintf("баз: %d", len(m.Databases))
		}
		if len(m.Volumes) > 0 {
			detail = fmt.Sprintf("томов: %d", len(m.Volumes))
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", mark, m.Kind, m.Name, trunc(detail, 60))
	}
	w.Flush()

	if !*apply {
		fmt.Println("\nЧтобы сохранить этот план: autobak server plan " + s.Name + " --apply")
		return nil
	}
	s.Plan = *p
	if err := a.Save(); err != nil {
		return err
	}
	fmt.Println("\nПлан сохранён. Запустить бэкап: autobak backup " + s.Name)
	return nil
}

// --- операции -------------------------------------------------------------

func cmdBackup(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("укажите имя сервера")
	}
	a, err := openApp()
	if err != nil {
		return err
	}
	start := time.Now()
	snap, err := a.Backup(ctx, args[0], events())
	clearLine()
	if err != nil {
		return err
	}
	fmt.Printf("Снимок %s готов за %s\n", snap.ID, time.Since(start).Round(time.Second))
	fmt.Printf("  файлов: %d, каталогов: %d\n", snap.Stats.Files, snap.Stats.Dirs)
	fmt.Printf("  прочитано: %s, новых данных: %s, записано в хранилище: %s\n",
		repo.HumanBytes(snap.Stats.BytesTotal),
		repo.HumanBytes(snap.Stats.BytesNew),
		repo.HumanBytes(snap.Stats.BytesStored))
	if !snap.Complete() {
		fmt.Println("\nСнимок неполон:")
		for _, m := range snap.Failed() {
			fmt.Printf("  ! %s: %s\n", m.Name, m.Err)
		}
		return errors.New("часть модулей не отработала")
	}
	return nil
}

func cmdSnapshots(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("укажите имя сервера")
	}
	a, err := openApp()
	if err != nil {
		return err
	}
	snaps, err := a.Snapshots(ctx, args[0])
	if err != nil {
		return err
	}
	if len(snaps) == 0 {
		fmt.Println("Снимков пока нет.")
		return nil
	}
	w := tw()
	fmt.Fprintln(w, "СНИМОК\tКОГДА\tФАЙЛОВ\tОБЪЁМ\tПРИРОСТ\tСОСТОЯНИЕ")
	for _, s := range snaps {
		state := "полный"
		if !s.Complete() {
			state = "неполный"
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\t%s\n",
			s.ID, s.Time.Local().Format("02.01.2006 15:04"), s.Stats.Files,
			repo.HumanBytes(s.Stats.BytesTotal),
			repo.HumanBytes(s.Stats.BytesStored), state)
	}
	return w.Flush()
}

func cmdRestore(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	toServer := fs.Bool("to-server", false, "вернуть данные на сервер (иначе скачать сюда)")
	dir := fs.String("dir", "", "куда скачивать")
	include := fs.String("include", "", "восстановить только эти пути (через запятую)")
	overwrite := fs.Bool("overwrite", false, "разрешить перезапись существующего")
	dbMode := fs.String("db", "file", "дампы баз: skip|file|restore")
	dbInPlace := fs.Bool("db-in-place", false, "залить базы поверх боевых")
	apply := fs.Bool("apply", false, "выполнить (без этого - только сухой прогон)")
	if err := fs.Parse(hoistFlags(args)); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		return errors.New("укажите: restore <сервер> <снимок>")
	}
	a, err := openApp()
	if err != nil {
		return err
	}
	s, err := a.Config().Server(fs.Arg(0))
	if err != nil {
		return err
	}

	opt := app.RestoreOptions{
		SnapshotID: fs.Arg(1),
		ToServer:   *toServer,
		LocalDir:   *dir,
		Overwrite:  *overwrite,
		DBMode:     restore.DBMode(*dbMode),
		DBInPlace:  *dbInPlace,
		DryRun:     true,
	}
	if *include != "" {
		opt.Include = strings.Split(*include, ",")
	}

	// Сухой прогон выполняется всегда - даже когда просили применить.
	// Человек должен увидеть объём последствий до, а не после.
	rep, err := a.Restore(ctx, s.ID, opt, events())
	clearLine()
	if err != nil {
		return err
	}
	fmt.Println("Будет восстановлено: " + rep.Summary())
	for _, p := range rep.Problems {
		fmt.Println("  ! " + p)
	}
	if n := len(rep.Overwrites); n > 0 {
		fmt.Printf("\nБудет перезаписано файлов: %d, среди них:\n", n)
		for _, p := range rep.Overwrites[:min(10, n)] {
			fmt.Println("  " + p)
		}
	}
	if *dbInPlace {
		fmt.Println("\n!! Базы будут залиты ПОВЕРХ боевых. Текущие данные исчезнут.")
	}
	if !*apply {
		fmt.Println("\nЭто был сухой прогон. Для выполнения добавьте --apply")
		return nil
	}

	if *toServer || *overwrite || *dbInPlace {
		if err := confirmName("сервера", s.Name); err != nil {
			return err
		}
	}
	opt.DryRun = false
	rep, err = a.Restore(ctx, s.ID, opt, events())
	clearLine()
	if err != nil {
		return err
	}
	fmt.Println("Восстановлено: " + rep.Summary())
	return nil
}

func cmdPrune(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("prune", flag.ContinueOnError)
	apply := fs.Bool("apply", false, "выполнить удаление")
	if err := fs.Parse(hoistFlags(args)); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("укажите имя сервера")
	}
	a, err := openApp()
	if err != nil {
		return err
	}
	s, err := a.Config().Server(fs.Arg(0))
	if err != nil {
		return err
	}
	fmt.Printf("Политика хранения: %s\n", s.Retention.Describe())

	rep, err := a.Prune(ctx, s.ID, true, events())
	clearLine()
	if err != nil {
		return err
	}
	fmt.Println(rep.Summary())
	if !*apply {
		fmt.Println("\nЭто был сухой прогон. Для выполнения добавьте --apply")
		return nil
	}
	if len(rep.SnapshotsRemoved) > 0 {
		if err := confirmName("сервера", s.Name); err != nil {
			return err
		}
	}
	rep, err = a.Prune(ctx, s.ID, false, events())
	clearLine()
	if err != nil {
		return err
	}
	fmt.Println(rep.Summary())
	return nil
}

func cmdVerify(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	sample := fs.Float64("sample", 0.05, "доля читаемых чанков (1 - полная проверка)")
	if err := fs.Parse(hoistFlags(args)); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("укажите имя хранилища")
	}
	a, err := openApp()
	if err != nil {
		return err
	}
	rep, err := a.Verify(ctx, fs.Arg(0), *sample, events())
	clearLine()
	if err != nil {
		return err
	}
	fmt.Println(rep.Summary())
	for _, p := range rep.Problems {
		fmt.Println("  ! " + p)
	}
	if !rep.OK() {
		return errors.New("репозиторий повреждён")
	}
	return nil
}

// cmdSchedule запускает всё, у чего подошло время.
//
// Отдельной службы нет намеренно: планировщик задач Windows надёжнее
// собственного демона, переживает перезагрузку и виден в системе.
// Программа лишь отвечает на вопрос «что пора делать прямо сейчас».
func cmdSchedule(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] != "run" {
		return errors.New("укажите: schedule run")
	}
	a, err := openApp()
	if err != nil {
		return err
	}
	// Сама процедура живёт в ядре: её же вызывает встроенный таймер
	// веб-интерфейса. Здесь остаётся только вывод в терминал.
	ev := events()
	ev.Log = func(level, msg string) {
		fmt.Printf("\r\033[K[%s] %s\n", level, msg)
	}
	return a.RunDue(ctx, ev)
}
