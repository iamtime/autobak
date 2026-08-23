package restore

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/iamtime/autobak/internal/collect"
	"github.com/iamtime/autobak/internal/repo"
)

type DBMode string

const (
	// DBSkip не трогает базы вовсе. Разумно, когда восстанавливают только файлы.
	DBSkip DBMode = "skip"
	// DBFile кладёт дампы файлами. Безопасный вариант по умолчанию:
	// человек посмотрит, что внутри, и зальёт сам.
	DBFile DBMode = "file"
	// DBRestore заливает дампы в СУБД.
	DBRestore DBMode = "restore"
)

type DBOptions struct {
	Mode DBMode
	// Suffix добавляется к имени базы: shop → shop_restore_20260821.
	// Так восстановление не задевает работающую базу, и результат можно
	// сравнить с боевой до того, как что-то переключать.
	Suffix string
	// InPlace заливает поверх исходной базы. Необратимо, поэтому включается
	// только явным решением человека, а не значением по умолчанию.
	InPlace bool

	Log func(level, msg string)
}

func (o DBOptions) logf(level, format string, args ...any) {
	if o.Log != nil {
		o.Log(level, fmt.Sprintf(format, args...))
	}
}

// TargetName показывает, куда попадёт база. Нужен интерфейсу, чтобы
// написать это в окне подтверждения прямым текстом.
func (o DBOptions) TargetName(db string) string {
	if o.InPlace {
		return db
	}
	return db + o.Suffix
}

// NewDBHandler возвращает обработчик виртуальных путей для fsTarget.
func NewDBHandler(ctx context.Context, o DBOptions) func(*repo.Node, io.Reader) (bool, error) {
	return func(n *repo.Node, content io.Reader) (bool, error) {
		switch {
		case strings.HasPrefix(n.Path, collect.VirtualMySQL+"/"):
			return o.handleMySQL(ctx, n, content)
		case strings.HasPrefix(n.Path, collect.VirtualPostgres+"/"):
			return o.handlePostgres(ctx, n, content)
		}
		// Всё остальное (/@docker/...) обрабатывается как обычные файлы.
		return false, nil
	}
}

func (o DBOptions) handleMySQL(ctx context.Context, n *repo.Node, content io.Reader) (bool, error) {
	if o.Mode != DBRestore {
		return o.fallthroughOrSkip(n, content)
	}
	name := strings.TrimSuffix(strings.TrimPrefix(n.Path, collect.VirtualMySQL+"/"), ".sql")
	if strings.HasPrefix(name, "@") {
		// Права восстанавливаются только вместе с боевыми базами: выдавать
		// пользователям доступ к базе-копии - прямой способ получить
		// приложение, незаметно работающее не с той базой.
		if !o.InPlace {
			o.logf("info", "права пользователей пропущены: базы восстановлены под другими именами")
			_, _ = io.Copy(io.Discard, content)
			return true, nil
		}
		return true, o.pipeMySQL(ctx, "", content)
	}

	target := o.TargetName(name)
	if err := safeDBName(target); err != nil {
		return true, err
	}
	if err := o.execMySQL(ctx, fmt.Sprintf(
		"CREATE DATABASE IF NOT EXISTS `%s` CHARACTER SET utf8mb4", escapeIdent(target))); err != nil {
		return true, err
	}
	o.logf("info", "база %s восстанавливается в %s", name, target)
	// Из дампа убираются CREATE DATABASE и USE: без этого содержимое
	// уехало бы в исходную базу мимо выбранного имени.
	return true, o.pipeMySQL(ctx, target, stripDatabaseStatements(content))
}

func (o DBOptions) handlePostgres(ctx context.Context, n *repo.Node, content io.Reader) (bool, error) {
	if o.Mode != DBRestore {
		return o.fallthroughOrSkip(n, content)
	}
	base := strings.TrimPrefix(n.Path, collect.VirtualPostgres+"/")
	if strings.HasPrefix(base, "@") {
		if !o.InPlace {
			o.logf("info", "роли PostgreSQL пропущены: базы восстановлены под другими именами")
			_, _ = io.Copy(io.Discard, content)
			return true, nil
		}
		cmd := pgCommand(ctx, "psql", "-q")
		return true, pipeInto(cmd, content)
	}

	name := strings.TrimSuffix(base, ".dump")
	target := o.TargetName(name)
	if err := safeDBName(target); err != nil {
		return true, err
	}
	if out, err := pgCommand(ctx, "createdb", "--", target).CombinedOutput(); err != nil &&
		!bytes.Contains(out, []byte("already exists")) {
		return true, fmt.Errorf("не создать базу %s: %w: %s", target, err, out)
	}
	o.logf("info", "база %s восстанавливается в %s", name, target)
	cmd := pgCommand(ctx, "pg_restore", "--no-owner", "--no-acl", "-d", target)
	return true, pipeInto(cmd, content)
}

// fallthroughOrSkip решает судьбу дампа, когда заливать в СУБД не просили.
func (o DBOptions) fallthroughOrSkip(n *repo.Node, content io.Reader) (bool, error) {
	if o.Mode == DBSkip {
		_, _ = io.Copy(io.Discard, content)
		return true, nil
	}
	return false, nil // DBFile: пусть ляжет обычным файлом
}

func (o DBOptions) execMySQL(ctx context.Context, sql string) error {
	bin, err := mysqlBinary()
	if err != nil {
		return err
	}
	out, err := exec.CommandContext(ctx, bin, "-e", sql).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (o DBOptions) pipeMySQL(ctx context.Context, db string, content io.Reader) error {
	bin, err := mysqlBinary()
	if err != nil {
		return err
	}
	args := []string{}
	if db != "" {
		args = append(args, db)
	}
	return pipeInto(exec.CommandContext(ctx, bin, args...), content)
}

func pipeInto(cmd *exec.Cmd, content io.Reader) error {
	var errBuf bytes.Buffer
	cmd.Stdin = content
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errBuf.String())
		if len(msg) > 1500 {
			msg = msg[:1500] + "..."
		}
		return fmt.Errorf("%s: %w: %s", cmd.Path, err, msg)
	}
	return nil
}

func mysqlBinary() (string, error) {
	for _, name := range []string{"mysql", "mariadb"} {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("autobak: на сервере нет клиента mysql")
}

// escapeIdent защищает имя базы, попадающее в SQL. Имена приходят из
// снимка, но проходят через интерфейс, где их можно отредактировать.
func escapeIdent(s string) string {
	return strings.ReplaceAll(s, "`", "``")
}

// safeDBName отклоняет имя базы, которым можно провести инъекцию опции в
// createdb/mysql: имя-аргумент, начинающееся с "-", клиент СУБД примет за
// флаг. Имя восстанавливаемой базы приходит из пути в снимке и может быть
// отредактировано в интерфейсе - проверяем до передачи в команду.
func safeDBName(name string) error {
	if name == "" {
		return fmt.Errorf("autobak: пустое имя базы")
	}
	if strings.HasPrefix(name, "-") {
		return fmt.Errorf("autobak: имя базы %q начинается с дефиса - отклонено во избежание инъекции опции", name)
	}
	if strings.ContainsAny(name, "\x00\n\r/\\") {
		return fmt.Errorf("autobak: имя базы %q содержит недопустимый символ", name)
	}
	return nil
}

// stripDatabaseStatements убирает из дампа команды переключения базы.
//
// mysqldump --databases всегда вписывает CREATE DATABASE и USE с исходным
// именем. Без их удаления восстановление «в копию» молча заливало бы
// данные обратно в боевую базу - ровно та ошибка, от которой безопасный
// режим и должен защищать.
func stripDatabaseStatements(r io.Reader) io.Reader {
	pr, pw := io.Pipe()
	go func() {
		br := bufio.NewReaderSize(r, 256<<10)
		var err error
		for {
			// ReadBytes, а не Scanner: строки INSERT в дампах бывают
			// многомегабайтными, и лимит буфера Scanner на них падает.
			line, rerr := br.ReadBytes('\n')
			if len(line) > 0 {
				if !isDatabaseStatement(line) {
					if _, werr := pw.Write(line); werr != nil {
						err = werr
						break
					}
				}
			}
			if rerr != nil {
				if rerr != io.EOF {
					err = rerr
				}
				break
			}
		}
		pw.CloseWithError(err)
	}()
	return pr
}

func isDatabaseStatement(line []byte) bool {
	t := bytes.ToUpper(bytes.TrimLeft(line, " \t"))
	return bytes.HasPrefix(t, []byte("CREATE DATABASE")) ||
		bytes.HasPrefix(t, []byte("DROP DATABASE")) ||
		bytes.HasPrefix(t, []byte("USE "))
}
