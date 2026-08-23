package collect

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/iamtime/autobak/internal/plan"
)

// systemDBs не бэкапятся: information_schema и performance_schema -
// виртуальные представления, mysql и sys восстанавливаются установкой
// сервера, а их перенос между версиями ломает права.
var systemDBs = []string{"information_schema", "performance_schema", "sys", "mysql"}

type mysqlCollector struct {
	m    plan.Module
	dump string
	cli  string
	// defaultsFile - временный файл с доступами в формате my.cnf.
	// Пароль передаётся только так: аргументы командной строки видны
	// в ps любому пользователю сервера.
	defaultsFile string
	dbs          []string

	helpOnce sync.Once
	help     string
}

func newMySQL(m plan.Module) *mysqlCollector { return &mysqlCollector{m: m} }

func (c *mysqlCollector) Kind() plan.Kind { return plan.KindMySQL }
func (c *mysqlCollector) Name() string    { return c.m.Name }

func (c *mysqlCollector) Collect(ctx context.Context, s Sink) (map[string]any, error) {
	var err error
	if c.dump, err = mysqlDumpBinary(); err != nil {
		return nil, err
	}
	if c.cli, err = mysqlClientBinary(); err != nil {
		return nil, err
	}
	if err := c.setupCredentials(s); err != nil {
		return nil, err
	}
	if c.defaultsFile != "" {
		defer os.Remove(c.defaultsFile)
	}

	dbs := c.m.Databases
	if len(dbs) == 0 {
		if dbs, err = c.listDatabases(ctx); err != nil {
			return nil, err
		}
	}
	c.dbs = dbs
	if len(dbs) == 0 {
		s.Logf("warn", "не найдено ни одной базы MySQL для бэкапа")
		return map[string]any{"databases": dbs}, nil
	}

	// Права пользователей идут первыми: без них восстановленные базы
	// окажутся недоступны приложениям, и это выяснится в худший момент.
	if err := c.dumpGrants(ctx, s); err != nil {
		s.Logf("warn", "не удалось сохранить права пользователей: %v", err)
	}

	var failed []string
	for _, db := range dbs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := c.dumpOne(ctx, s, db); err != nil {
			// Одна битая база не должна отменять бэкап остальных:
			// половина баз лучше, чем ни одной.
			failed = append(failed, db)
			s.Logf("error", "база %s не выгружена: %v", db, err)
		}
	}
	meta := map[string]any{"databases": dbs}
	if len(failed) > 0 {
		meta["failed"] = failed
		return meta, fmt.Errorf("autobak: не выгружены базы: %s", strings.Join(failed, ", "))
	}
	return meta, nil
}

func (c *mysqlCollector) dumpOne(ctx context.Context, s Sink, db string) error {
	args := c.baseArgs()
	args = append(args,
		// single-transaction снимает консистентный срез БЕЗ блокировки
		// таблиц - но только для InnoDB. Таблицы MyISAM/Aria транзакций не
		// поддерживают, и их дамп «на ходу» может оказаться несогласованным
		// между таблицами. На современных сайтах движок по умолчанию InnoDB;
		// про старые смешанные схемы это ограничение честно указано в
		// документации, а гарантировать консистентность MyISAM без простоя
		// нельзя в принципе.
		"--single-transaction",
		"--quick",    // не собирать таблицу в памяти целиком
		"--routines", // процедуры и функции
		"--triggers", //
		"--events",   //
		"--hex-blob", // бинарные поля переживут любую перекодировку
		"--default-character-set=utf8mb4",
	)
	// Эти опции есть только у mysqldump от Oracle. MariaDB на них не
	// ругается предупреждением, а завершается с ошибкой, то есть база
	// просто не попадает в бэкап. Поэтому спрашиваем саму утилиту,
	// что она умеет, вместо того чтобы разбирать текст ошибки постфактум.
	for _, opt := range []struct{ flag, arg string }{
		{"set-gtid-purged", "--set-gtid-purged=OFF"},
		{"column-statistics", "--column-statistics=0"},
	} {
		if c.supports(opt.flag) {
			args = append(args, opt.arg)
		}
	}
	if err := safeDBName(db); err != nil {
		return err
	}
	args = append(args, "--databases", db)

	n := virtualNode(path.Join(VirtualMySQL, db+".sql"), c.m.Name)
	return streamCommand(ctx, s, n, exec.CommandContext(ctx, c.dump, args...))
}

// supports сообщает, знает ли установленный mysqldump такую опцию.
//
// Вывод --help разбирается один раз за бэкап и запоминается: это один
// запуск процесса на весь прогон, а не на каждую базу.
func (c *mysqlCollector) supports(flag string) bool {
	c.helpOnce.Do(func() {
		// --help у mysqldump требует ещё и --verbose, чтобы напечатать
		// полный список опций; подключение к серверу при этом не нужно.
		out, _ := exec.Command(c.dump, "--verbose", "--help").Output()
		c.help = string(out)
	})
	return strings.Contains(c.help, "--"+flag)
}

// dumpGrants сохраняет пользователей и их права отдельным файлом.
func (c *mysqlCollector) dumpGrants(ctx context.Context, s Sink) error {
	users, err := c.query(ctx,
		"SELECT CONCAT(QUOTE(user), '@', QUOTE(host)) FROM mysql.user WHERE user <> ''")
	if err != nil {
		return err
	}
	var sb strings.Builder
	sb.WriteString("-- autobak: пользователи и права\n")
	for _, u := range users {
		grants, err := c.query(ctx, "SHOW GRANTS FOR "+u)
		if err != nil {
			continue // пользователь мог исчезнуть между запросами
		}
		sb.WriteString("\n-- " + u + "\n")
		for _, g := range grants {
			sb.WriteString(g + ";\n")
		}
	}
	sb.WriteString("\nFLUSH PRIVILEGES;\n")

	n := virtualNode(path.Join(VirtualMySQL, "@grants.sql"), c.m.Name)
	return s.File(n, strings.NewReader(sb.String()))
}

func (c *mysqlCollector) baseArgs() []string {
	var args []string
	if c.defaultsFile != "" {
		// Обязательно первым аргументом - так требует сам клиент MySQL.
		args = append(args, "--defaults-extra-file="+c.defaultsFile)
	}
	if c.m.Socket != "" {
		args = append(args, "--socket="+c.m.Socket)
	} else if c.m.Host != "" {
		args = append(args, "--host="+c.m.Host)
		if c.m.Port > 0 {
			args = append(args, fmt.Sprintf("--port=%d", c.m.Port))
		}
	}
	return args
}

func (c *mysqlCollector) query(ctx context.Context, sql string) ([]string, error) {
	args := append(c.baseArgs(), "--batch", "--skip-column-names", "--execute="+sql)
	out, err := runCapture(ctx, c.cli, args...)
	if err != nil {
		return nil, err
	}
	return trimLines(out), nil
}

func (c *mysqlCollector) listDatabases(ctx context.Context) ([]string, error) {
	names, err := c.query(ctx, "SHOW DATABASES")
	if err != nil {
		return nil, fmt.Errorf("autobak: не получить список баз: %w", err)
	}
	var out []string
	for _, n := range names {
		if !slices.Contains(systemDBs, n) {
			out = append(out, n)
		}
	}
	return out, nil
}

// setupCredentials ищет доступ к серверу баз в том порядке, в каком его
// стоит искать на реальном сервере, и складывает найденное во временный
// файл с правами 0600.
func (c *mysqlCollector) setupCredentials(s Sink) error {
	user, pass, sock := c.m.User, "", c.m.Socket

	// 1. Конфигурация, положенная установщиком autobak. Приоритет у неё:
	//    так администратор может завести отдельного пользователя только
	//    для бэкапов, с правами лишь на чтение.
	if u, p, h := readMyCnf("/etc/autobak/mysql.cnf"); u != "" {
		user, pass = u, p
		if sock == "" {
			sock = h
		}
		s.Logf("info", "доступ к MySQL взят из /etc/autobak/mysql.cnf")
	} else if u, p := readHestiaMySQL("/usr/local/hestia/conf/mysql.conf"); u != "" {
		// 2. HestiaCP хранит пароль root в своей конфигурации.
		user, pass = u, p
		s.Logf("info", "доступ к MySQL взят из конфигурации HestiaCP")
	} else if _, err := os.Stat("/root/.my.cnf"); err == nil {
		// 3. Клиент сам подхватит /root/.my.cnf - вмешиваться не нужно.
		s.Logf("info", "используется /root/.my.cnf")
		return nil
	} else if user == "" {
		// 4. Аутентификация через unix-сокет: на современных Debian/Ubuntu
		//    root подключается к MariaDB вообще без пароля.
		s.Logf("info", "подключение к MySQL через unix-сокет от имени root")
		return nil
	}

	if pass == "" && user == "" {
		return nil
	}
	f, err := os.CreateTemp("", "autobak-my-*.cnf")
	if err != nil {
		return fmt.Errorf("autobak: не создать файл доступов: %w", err)
	}
	// Права сужаются до владельца до записи пароля, а не после.
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		os.Remove(f.Name())
		return err
	}
	fmt.Fprintf(f, "[client]\nuser=%s\npassword=%s\n", user, pass)
	if sock != "" {
		fmt.Fprintf(f, "socket=%s\n", sock)
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return err
	}
	c.defaultsFile = f.Name()
	return nil
}

var myCnfLine = regexp.MustCompile(`(?m)^\s*(user|password|socket|host)\s*=\s*"?'?([^"'\r\n]*)"?'?\s*$`)

func readMyCnf(path string) (user, pass, socket string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", ""
	}
	for _, m := range myCnfLine.FindAllStringSubmatch(string(data), -1) {
		switch m[1] {
		case "user":
			user = m[2]
		case "password":
			pass = m[2]
		case "socket":
			socket = m[2]
		}
	}
	return user, pass, socket
}

var hestiaVar = regexp.MustCompile(`(?m)^\s*(HOST|USER|PASSWORD)\s*=\s*'?"?([^'"\r\n]*)'?"?`)

func readHestiaMySQL(path string) (user, pass string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", ""
	}
	for _, m := range hestiaVar.FindAllStringSubmatch(string(data), -1) {
		switch m[1] {
		case "USER":
			user = m[2]
		case "PASSWORD":
			pass = m[2]
		}
	}
	return user, pass
}

// mysqldump в Debian 12+ и на MariaDB называется по-разному, поэтому
// перебираем известные имена, а не полагаемся на одно.
func mysqlDumpBinary() (string, error) {
	for _, name := range []string{"mysqldump", "mariadb-dump"} {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("autobak: на сервере нет mysqldump - установите mysql-client или mariadb-client")
}

func mysqlClientBinary() (string, error) {
	for _, name := range []string{"mysql", "mariadb"} {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("autobak: на сервере нет клиента mysql - установите mysql-client или mariadb-client")
}
