package collect

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"slices"
	"strings"

	"github.com/iamtime/autobak/internal/plan"
)

type postgresCollector struct {
	m plan.Module
	// asPostgres оборачивает команду в su, если агент работает от root:
	// PostgreSQL по умолчанию пускает без пароля только системного
	// пользователя postgres (peer-аутентификация).
	wrap func(ctx context.Context, bin string, args ...string) *exec.Cmd
}

func newPostgres(m plan.Module) *postgresCollector { return &postgresCollector{m: m} }

func (c *postgresCollector) Kind() plan.Kind { return plan.KindPostgres }
func (c *postgresCollector) Name() string    { return c.m.Name }

func (c *postgresCollector) Collect(ctx context.Context, s Sink) (map[string]any, error) {
	if _, err := lookPath("pg_dump", "установите postgresql-client"); err != nil {
		return nil, err
	}
	c.setupWrapper(s)

	dbs := c.m.Databases
	if len(dbs) == 0 {
		var err error
		if dbs, err = c.listDatabases(ctx); err != nil {
			return nil, err
		}
	}
	if len(dbs) == 0 {
		s.Logf("warn", "не найдено ни одной базы PostgreSQL")
		return map[string]any{"databases": dbs}, nil
	}

	// Роли и права хранятся на уровне кластера, а не базы: без globals
	// восстановленные базы окажутся ничьими.
	if err := c.dumpGlobals(ctx, s); err != nil {
		s.Logf("warn", "не удалось сохранить роли: %v", err)
	}

	var failed []string
	for _, db := range dbs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := c.dumpOne(ctx, s, db); err != nil {
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

func (c *postgresCollector) dumpOne(ctx context.Context, s Sink, db string) error {
	if err := safeDBName(db); err != nil {
		return err
	}
	n := virtualNode(path.Join(VirtualPostgres, db+".dump"), c.m.Name)
	// -Z0 отключает собственное сжатие pg_dump. Это не небрежность: сжатый
	// им поток меняется целиком от любой правки, и дедупликация перестаёт
	// работать - каждый снимок стоил бы как полный. Сжимаем мы сами, после
	// нарезки на чанки, и получаем и сжатие, и инкрементальность.
	cmd := c.wrap(ctx, "pg_dump", "--format=custom", "-Z0", "--no-password", db)
	return streamCommand(ctx, s, n, cmd)
}

func (c *postgresCollector) dumpGlobals(ctx context.Context, s Sink) error {
	n := virtualNode(path.Join(VirtualPostgres, "@globals.sql"), c.m.Name)
	return streamCommand(ctx, s, n, c.wrap(ctx, "pg_dumpall", "--globals-only", "--no-password"))
}

func (c *postgresCollector) listDatabases(ctx context.Context) ([]string, error) {
	cmd := c.wrap(ctx, "psql", "-At", "-c",
		"SELECT datname FROM pg_database WHERE NOT datistemplate AND datallowconn")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("autobak: не получить список баз PostgreSQL: %w", err)
	}
	var res []string
	for _, db := range trimLines(out) {
		if !slices.Contains([]string{"postgres"}, db) {
			res = append(res, db)
		}
	}
	return res, nil
}

func (c *postgresCollector) setupWrapper(s Sink) {
	host, port, user := c.m.Host, c.m.Port, c.m.User

	direct := func(ctx context.Context, bin string, args ...string) *exec.Cmd {
		full := args
		if host != "" {
			full = append([]string{"--host=" + host}, full...)
		}
		if port > 0 {
			full = append([]string{fmt.Sprintf("--port=%d", port)}, full...)
		}
		if user != "" {
			full = append([]string{"--username=" + user}, full...)
		}
		return exec.CommandContext(ctx, bin, full...)
	}

	if os.Geteuid() != 0 || host != "" {
		c.wrap = direct
		return
	}
	// От root подключение идёт через su postgres: иначе peer-аутентификация
	// отвергнет соединение, а пароль в конфиге хранить не хочется.
	s.Logf("info", "PostgreSQL опрашивается от имени системного пользователя postgres")
	c.wrap = func(ctx context.Context, bin string, args ...string) *exec.Cmd {
		quoted := make([]string, 0, len(args)+1)
		quoted = append(quoted, bin)
		for _, a := range args {
			quoted = append(quoted, shellQuote(a))
		}
		return exec.CommandContext(ctx, "su", "-s", "/bin/sh", "postgres", "-c", strings.Join(quoted, " "))
	}
}

// shellQuote заключает аргумент в одинарные кавычки. Имена баз приходят
// из плана, то есть в конечном счёте от пользователя, и попадают в
// командную строку su - без экранирования это была бы инъекция команд.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}
