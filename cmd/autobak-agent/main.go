// Команда autobak-agent - единственное, что ставится на сервер.
//
// Не демон: портов не слушает, в фоне не висит. Запускается либо по SSH
// (десктоп выполняет его как команду и общается через stdin/stdout), либо
// системным таймером для бэкапа по расписанию.
//
// В authorized_keys ключ прописывается так:
//
//	command="/usr/local/bin/autobak-agent serve",restrict ssh-ed25519 AAAA...
//
// Тогда даже украденный ключ не даёт shell - только команды из белого
// списка ниже.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"

	"github.com/iamtime/autobak/internal/collect"
	"github.com/iamtime/autobak/internal/discover"
	"github.com/iamtime/autobak/internal/engine"
	"github.com/iamtime/autobak/internal/plan"
	"github.com/iamtime/autobak/internal/proto"
	"github.com/iamtime/autobak/internal/restore"
)

// Version подставляется при сборке: -ldflags "-X main.Version=1.2.3".
var Version = "dev"

// serveAllow - серверное ограничение на то, что план имеет право прочитать.
//
// Заполняется из аргументов "serve" в authorized_keys (--allow=..., --allow-db)
// и проверяется в export/backup. Живёт в пакетной переменной сознательно:
// процесс агента обслуживает ровно один запрос и тут же завершается,
// параллельных вызовов внутри одного процесса нет.
var serveAllow *plan.Allow

// allowedCommands - то, что можно вызвать через SSH.
//
// Белый список, а не чёрный: любая новая подкоманда по умолчанию
// недоступна снаружи, пока её сюда сознательно не добавят.
var allowedCommands = []string{"discover", "export", "import", "backup", "version", "selftest", "estimate"}

// backupOnlyCommands - то же без import.
//
// Разница принципиальная, и её стоит понимать. Команда import пишет
// произвольные файлы от root: она для того и нужна, чтобы возвращать
// сервер к жизни. Но это же означает, что ключ, которому она разрешена,
// равносилен root - им можно положить свой authorized_keys и получить
// настоящий shell.
//
// Ключ только для бэкапов такой возможности не даёт. Восстановление
// тогда требует руками разрешить import на время операции - и это
// правильное трение: восстановление поверх боевого сервера не должно
// быть чем-то, что случается само.
var backupOnlyCommands = []string{"discover", "export", "backup", "version", "selftest", "estimate"}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Args[1:]); err != nil {
		if errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, "autobak: прервано")
			os.Exit(130)
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		usage()
		return errors.New("autobak: не указана команда")
	}

	switch args[0] {
	case "serve":
		// Разбор без flag: аргументы приходят из authorized_keys, где
		// строка задаётся администратором, и лишняя гибкость здесь
		// только увеличивает поверхность.
		return serve(ctx, args[1:])
	case "discover":
		return cmdDiscover(ctx, args[1:])
	case "export":
		return cmdExport(ctx, args[1:])
	case "import":
		return cmdImport(ctx, args[1:])
	case "backup":
		return cmdBackup(ctx, args[1:])
	case "estimate":
		return cmdEstimate(ctx, args[1:])
	case "selftest":
		return cmdSelftest(ctx)
	case "version", "--version", "-v":
		fmt.Printf("autobak-agent %s\n", Version)
		return nil
	case "help", "--help", "-h":
		usage()
		return nil
	}
	return fmt.Errorf("autobak: неизвестная команда %q", args[0])
}

func usage() {
	fmt.Fprint(os.Stderr, `autobak-agent - агент резервного копирования

  discover [--json]         показать карту сервера
  export   --plan <файл>    отдать данные потоком в stdout
  import   [--root /]       принять поток восстановления из stdin
  backup   --config <файл>  выполнить бэкап по расписанию (push-режим)
  selftest                  проверить, что всё нужное на сервере есть
  version

Через SSH вызывается как "serve": команда берётся из SSH_ORIGINAL_COMMAND.
С "serve --backup-only" восстановление запрещено - такой ключ не даёт
писать в файловую систему сервера.
`)
}

// serve - точка входа при вызове по SSH.
//
// Реальная команда приходит в SSH_ORIGINAL_COMMAND, потому что в
// authorized_keys жёстко прописан вызов "serve". Разбор нарочно
// примитивный, без shell-семантики: никаких кавычек, подстановок и
// конвейеров, только слова через пробел. Всё, что не в белом списке,
// отвергается до того, как хоть что-то произойдёт.
func serve(ctx context.Context, serveArgs []string) error {
	backupOnly := slices.Contains(serveArgs, "--backup-only")
	allowed := allowedCommands
	if backupOnly {
		allowed = backupOnlyCommands
	}
	// Серверное ограничение на доступ к данным. Оно из authorized_keys, то
	// есть с доверенной стороны, и клиентский план его расширить не может.
	serveAllow = plan.ParseAllow(serveArgs)

	raw := os.Getenv("SSH_ORIGINAL_COMMAND")
	if raw == "" {
		return errors.New("autobak: команда не передана (SSH_ORIGINAL_COMMAND пуст)")
	}
	for _, ch := range []string{";", "|", "&", "`", "$(", ">", "<", "\n"} {
		if strings.Contains(raw, ch) {
			return fmt.Errorf("autobak: в команде недопустимый символ %q", ch)
		}
	}
	args := strings.Fields(raw)
	if len(args) == 0 {
		return errors.New("autobak: пустая команда")
	}
	// Клиент вызывает "autobak-agent export ...", отбрасываем имя программы.
	if strings.HasSuffix(args[0], "autobak-agent") {
		args = args[1:]
	}
	if len(args) == 0 || !slices.Contains(allowed, args[0]) {
		if backupOnly && len(args) > 0 && args[0] == "import" {
			return errors.New("autobak: этот ключ выдан только для бэкапов. " +
				"Чтобы восстановить, замените в authorized_keys " +
				`command="... serve --backup-only" на command="... serve"`)
		}
		return fmt.Errorf("autobak: команда %q не разрешена этим ключом", strings.Join(args, " "))
	}
	return run(ctx, args)
}

func cmdDiscover(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("discover", flag.ContinueOnError)
	asJSON := fs.Bool("json", true, "вывод в формате JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rep := discover.Run(ctx, Version)
	if !*asJSON {
		fmt.Println(rep.Summary())
		return nil
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(rep)
}

// cmdExport отдаёт данные сервера потоком.
//
// По умолчанию запрос читается кадром со стандартного ввода, и канал
// остаётся открытым: по нему десктоп отвечает, какие чанки у него уже
// есть, и агент не передаёт их повторно.
//
// Вариант с --plan оставлен для отладки руками: файл с планом, весь поток
// целиком, без обратного канала.
// cmdEstimate быстро оценивает объём бэкапа по плану, не читая содержимое.
//
// План приходит JSON-ом на stdin. Ответ - JSON с числом байт и файлов.
// Нужен интерфейсу, чтобы показать «сколько примерно места» до запуска.
func cmdEstimate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("estimate", flag.ContinueOnError)
	planFile := fs.String("plan", "", "файл с планом (по умолчанию читается из stdin)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var raw []byte
	var err error
	if *planFile != "" {
		raw, err = os.ReadFile(*planFile)
	} else {
		raw, err = io.ReadAll(io.LimitReader(os.Stdin, 8<<20))
	}
	if err != nil {
		return fmt.Errorf("autobak: не прочитать план: %w", err)
	}
	var p plan.Plan
	if err := json.Unmarshal(raw, &p); err != nil {
		return fmt.Errorf("autobak: план повреждён: %w", err)
	}
	// То же серверное ограничение, что и у export: оценка тоже обходит
	// каталоги, и украденный ключ не должен считать размер /etc/shadow.
	if err := p.CheckAllowed(serveAllow); err != nil {
		return err
	}
	est := collect.EstimatePlan(ctx, &p)
	return json.NewEncoder(os.Stdout).Encode(est)
}

func cmdExport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	planFile := fs.String("plan", "", "файл с планом (для отладки; без него запрос читается кадром из stdin)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	var req proto.Request
	if *planFile != "" {
		raw, err := os.ReadFile(*planFile)
		if err != nil {
			return fmt.Errorf("autobak: не прочитать план: %w", err)
		}
		req.Plan = raw
	} else {
		r := proto.NewReader(os.Stdin)
		t, payload, err := r.Next()
		if err != nil {
			return fmt.Errorf("autobak: не получен запрос от десктопа: %w", err)
		}
		if t != proto.FrameRequest {
			return fmt.Errorf("autobak: ожидался запрос, получен кадр %s", t)
		}
		if req, err = proto.DecodeJSON[proto.Request](payload); err != nil {
			return fmt.Errorf("autobak: запрос повреждён: %w", err)
		}
	}

	var p plan.Plan
	if err := json.Unmarshal(req.Plan, &p); err != nil {
		return fmt.Errorf("autobak: план повреждён: %w", err)
	}
	if err := p.Validate(); err != nil {
		return err
	}
	// Серверное ограничение проверяется здесь, до чтения хоть одного байта:
	// украденный ключ не должен уметь запросить выгрузку /etc/shadow или
	// ключа репозитория, даже если пришлёт план с такими путями.
	if err := p.CheckAllowed(serveAllow); err != nil {
		return err
	}
	applyPriority(&p)
	return engine.Export(ctx, os.Stdout, os.Stdin, &req, Version)
}

func cmdImport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	root := fs.String("root", "", "каталог назначения (пусто - восстановление на исходные места)")
	overwrite := fs.Bool("overwrite", false, "разрешить перезапись существующих файлов")
	owner := fs.Bool("owner", true, "восстанавливать владельца и группу")
	dbMode := fs.String("db", string(restore.DBFile), "что делать с дампами баз: skip|file|restore")
	dbSuffix := fs.String("db-suffix", "", "суффикс имени восстанавливаемой базы")
	dbInPlace := fs.Bool("db-in-place", false, "заливать базы поверх существующих")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Восстановление поверх боевых баз - необратимо. Требуем, чтобы это
	// решение было выражено явно, а не получилось само из умолчаний.
	if *dbInPlace && restore.DBMode(*dbMode) != restore.DBRestore {
		return errors.New("autobak: --db-in-place имеет смысл только с --db=restore")
	}

	dbOpts := restore.DBOptions{
		Mode: restore.DBMode(*dbMode), Suffix: *dbSuffix, InPlace: *dbInPlace,
		Log: logToStderr,
	}
	target := restore.NewFS(restore.FSOptions{
		Root: *root, Overwrite: *overwrite, RestoreOwner: *owner,
		Virtual: restore.NewDBHandler(ctx, dbOpts),
		Log:     logToStderr,
	})
	return restore.Apply(ctx, os.Stdin, target)
}

func logToStderr(level, msg string) {
	fmt.Fprintf(os.Stderr, "[%s] %s\n", level, msg)
}
