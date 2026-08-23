// Команда autobak - десктопное приложение.
//
// Без аргументов открывает окно. С аргументами работает как обычная
// утилита: то же самое ядро, тот же конфиг, те же подтверждения - чтобы
// всё, что делается мышью, можно было поставить в планировщик.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/iamtime/autobak/internal/app"
)

var Version = "dev"

func main() {
	app.Version = Version

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	args := os.Args[1:]
	if len(args) == 0 {
		args = []string{"ui"}
	}

	if err := dispatch(ctx, args); err != nil {
		if errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, "\nautobak: прервано")
			os.Exit(130)
		}
		fmt.Fprintln(os.Stderr, "Ошибка: "+err.Error())
		os.Exit(1)
	}
}

func dispatch(ctx context.Context, args []string) error {
	switch args[0] {
	case "ui":
		return runUI(ctx)
	case "repo":
		return cmdRepo(ctx, args[1:])
	case "server":
		return cmdServer(ctx, args[1:])
	case "backup":
		return cmdBackup(ctx, args[1:])
	case "snapshots":
		return cmdSnapshots(ctx, args[1:])
	case "restore":
		return cmdRestore(ctx, args[1:])
	case "prune":
		return cmdPrune(ctx, args[1:])
	case "verify":
		return cmdVerify(ctx, args[1:])
	case "mirror":
		return cmdMirror(ctx, args[1:])
	case "drill":
		return cmdDrill(ctx, args[1:])
	case "deploy":
		return cmdDeploy(ctx, args[1:])
	case "agent-config":
		return cmdAgentConfig(ctx, args[1:])
	case "schedule":
		return cmdSchedule(ctx, args[1:])
	case "version", "--version", "-v":
		fmt.Printf("autobak %s\n", Version)
		return nil
	case "help", "--help", "-h":
		usage()
		return nil
	}
	usage()
	return fmt.Errorf("неизвестная команда %q", args[0])
}

func usage() {
	fmt.Fprint(os.Stderr, `autobak - резервное копирование серверов

  autobak                          открыть окно программы

  repo add                         подключить хранилище (локальное или S3)
  repo list                        список хранилищ

  server add                       добавить сервер
  server list                      список серверов и состояние
  server discover <сервер>         показать, что нашлось на сервере
  server plan <сервер>             составить план бэкапа по найденному
  server install <сервер>          установить агента на сервер

  backup <сервер>                  сделать бэкап сейчас
  snapshots <сервер>               список снимков
  restore <сервер> <снимок>        восстановить (по умолчанию - сухой прогон)
  prune <сервер>                   применить политику хранения
  verify <хранилище>               проверить целостность
  drill <сервер> [снимок]          восстановить выборку и сверить - проверка «на деле»
  mirror <откуда> <куда>           скопировать репозиторий во второе хранилище
  deploy <сервер> --to <адрес>     развернуть снимок на новый сервер
  agent-config <сервер>            собрать config.json и key для push-режима

  schedule run                     выполнить всё, что подошло по расписанию

Подробности по команде: autobak <команда> --help
`)
}

func openApp() (*app.App, error) {
	a, err := app.Open()
	if err != nil {
		return nil, err
	}
	return a, nil
}
