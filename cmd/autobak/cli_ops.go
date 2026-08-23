package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"

	"github.com/iamtime/autobak/internal/app"
	"github.com/iamtime/autobak/internal/repo"
	"github.com/iamtime/autobak/internal/restore"
	"github.com/iamtime/autobak/internal/sshx"
)

// --- Зеркало --------------------------------------------------------------

func cmdMirror(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("mirror", flag.ContinueOnError)
	apply := fs.Bool("apply", false, "выполнить копирование (без этого - сухой прогон)")
	prune := fs.Bool("prune", false, "удалить из зеркала то, чего больше нет в источнике")
	verify := fs.Bool("verify", false, "прочитать зеркало целиком и проверить подписи")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		return errors.New("укажите: mirror <откуда> <куда>")
	}
	a, err := openApp()
	if err != nil {
		return err
	}

	opt := repo.DefaultMirrorOptions()
	opt.Prune = *prune
	opt.Verify = *verify
	opt.DryRun = !*apply

	rep, err := a.Mirror(ctx, fs.Arg(0), fs.Arg(1), opt, events())
	clearLine()
	if err != nil {
		return err
	}
	fmt.Println(rep.Summary())
	for _, p := range rep.Problems {
		fmt.Println("  ! " + p)
	}
	if n := len(rep.Orphans); n > 0 && !*prune {
		fmt.Printf("\nВ зеркале %d объектов, которых больше нет в источнике.\n", n)
		fmt.Println("Они остаются намеренно: зеркало нужно ровно на случай, когда")
		fmt.Println("с источником случилось плохое. Удалить: добавьте --prune")
	}
	if !*apply {
		fmt.Println("\nЭто был сухой прогон. Для выполнения добавьте --apply")
		return nil
	}
	if !rep.OK() {
		return errors.New("часть объектов не скопирована")
	}
	return nil
}

// --- Проверка восстановлением ---------------------------------------------

func cmdDrill(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("drill", flag.ContinueOnError)
	maxMB := fs.Int64("max-mb", 1024, "сколько мегабайт восстанавливать для проверки (0 - весь снимок)")
	dir := fs.String("dir", "", "куда восстанавливать (пусто - во временный каталог, который удалится)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("укажите имя сервера")
	}
	a, err := openApp()
	if err != nil {
		return err
	}

	opt := restore.DrillOptions{MaxBytes: *maxMB << 20, Dir: *dir}
	rep, err := a.Drill(ctx, fs.Arg(0), fs.Arg(1), opt, events())
	clearLine()
	if err != nil {
		return err
	}
	fmt.Println(rep.Summary())
	for _, m := range rep.Mismatches {
		fmt.Println("  ! " + m)
	}
	for _, p := range rep.Problems {
		fmt.Println("  ! " + p)
	}
	if !rep.OK() {
		return errors.New("снимок не прошёл проверку восстановлением")
	}
	return nil
}

// --- Развёртывание на новый сервер ----------------------------------------

func cmdDeploy(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("deploy", flag.ContinueOnError)
	host := fs.String("to", "", "адрес нового сервера")
	alias := fs.String("to-alias", "", "запись из ~/.ssh/config вместо адреса")
	port := fs.Int("port", 22, "порт")
	user := fs.String("user", "root", "пользователь")
	key := fs.String("key", "", "приватный ключ")
	binary := fs.String("binary", "", "путь к autobak-agent для установки на цель")
	snapshot := fs.String("snapshot", "", "какой снимок (по умолчанию последний)")
	only := fs.String("only", "", "что именно: configs,sites,databases,docker")
	force := fs.Bool("force", false, "разрешить развёртывание на непустой сервер")
	apply := fs.Bool("apply", false, "выполнить (без этого - сухой прогон)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("укажите сервер-источник: deploy <сервер> --to <адрес>")
	}
	if *host == "" && *alias == "" {
		return errors.New("укажите --to или --to-alias")
	}
	a, err := openApp()
	if err != nil {
		return err
	}

	opt := app.DefaultDeployOptions()
	opt.Source = fs.Arg(0)
	opt.SnapshotID = *snapshot
	opt.AgentBinary = *binary
	opt.Force = *force
	opt.Target = sshx.Target{
		Alias: *alias, Host: *host, Port: *port, User: *user, KeyPath: *key,
	}
	if *only != "" {
		opt.Configs, opt.Sites, opt.Databases, opt.Docker = false, false, false, false
		for _, part := range strings.Split(*only, ",") {
			switch strings.TrimSpace(part) {
			case "configs":
				opt.Configs = true
			case "sites":
				opt.Sites = true
			case "databases":
				opt.Databases = true
			case "docker":
				opt.Docker = true
			default:
				return fmt.Errorf("неизвестная часть %q", part)
			}
		}
	}

	// Сухой прогон выполняется всегда: он ставит агента, осматривает цель
	// и показывает, что именно произойдёт. Это дешевле, чем узнать о
	// непустой цели после того, как её перезаписали.
	opt.DryRun = true
	rep, err := a.Deploy(ctx, opt, events())
	clearLine()
	printDeploy(rep)
	if err != nil {
		return err
	}
	if !*apply {
		fmt.Println("\nЭто был сухой прогон. Для выполнения добавьте --apply")
		return nil
	}

	fmt.Printf("\nЭто перезапишет данные на %s. Наберите адрес цели для подтверждения: ",
		opt.Target.Label())
	opt.Confirm = prompt("")
	if opt.Confirm != opt.Target.Label() {
		return errors.New("отменено: адрес не совпал")
	}

	opt.DryRun = false
	rep, err = a.Deploy(ctx, opt, events())
	clearLine()
	printDeploy(rep)
	if err != nil {
		return err
	}
	if !rep.OK() {
		return errors.New("развёртывание завершилось с ошибками")
	}
	return nil
}

func printDeploy(rep *app.DeployReport) {
	if rep == nil {
		return
	}
	fmt.Printf("Снимок %s → %s\n\n", rep.Snapshot, rep.Target)
	w := tw()
	for _, s := range rep.Steps {
		mark := "OK "
		detail := s.Detail
		if !s.OK {
			mark = "!! "
			detail = s.Err
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", mark, s.Name, detail)
	}
	w.Flush()

	if len(rep.Checklist) > 0 {
		fmt.Println("\nОсталось сделать вручную:")
		for i, c := range rep.Checklist {
			fmt.Printf("  %d. %s\n", i+1, c)
		}
	}
	fmt.Println("\n" + rep.Summary())
}

// --- Обслуживание по расписанию -------------------------------------------

// --- Конфигурация для push-режима -----------------------------------------

func cmdAgentConfig(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("agent-config", flag.ContinueOnError)
	out := fs.String("dir", "", "куда сложить файлы (по умолчанию - в каталог настроек)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("укажите имя сервера")
	}
	a, err := openApp()
	if err != nil {
		return err
	}
	res, err := a.WriteAgentConfig(ctx, fs.Arg(0), *out)
	if err != nil {
		return err
	}

	fmt.Printf("Готово: %s\n", res.Dir)
	for _, f := range res.Files {
		fmt.Println("  " + f)
	}
	if len(res.Warnings) > 0 {
		fmt.Println()
		for _, w := range res.Warnings {
			fmt.Println("  ! " + w)
		}
	}
	fmt.Printf("\nСкопируйте оба файла на сервер и выполните там:\n\n  %s\n", res.Command)
	fmt.Println("\nЛибо, если разворачиваете через docker: положите их в ./autobak")
	fmt.Println("рядом с docker-compose.yml и выполните docker compose up -d")
	return nil
}
