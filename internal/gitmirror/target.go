package gitmirror

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/iamtime/autobak/internal/collect"
	"github.com/iamtime/autobak/internal/repo"
	"github.com/iamtime/autobak/internal/restore"
)

// gitTarget раскладывает выбранное содержимое снимка в рабочий каталог git.
//
// Отличается от обычного восстановления тем, что здесь важна не точность
// воспроизведения (владельцы, права, устройства), а читаемость diff.
// Поэтому: только текст, только небольшое, никаких секретов, и дампы баз
// урезаются до структуры.
type gitTarget struct {
	dir string
	cfg *Config

	files   int
	skipped int
	secrets []string
}

func (t *gitTarget) Node(n *repo.Node, content io.Reader) error {
	if n.Type != repo.NodeFile {
		if n.Type == repo.NodeSymlink {
			// Ссылка сохраняется текстовым файлом: в git она была бы
			// отдельным типом объекта, а увидеть, куда она указывала,
			// важнее, чем воспроизвести её как ссылку.
			return t.write(n.Path+".symlink", []byte(n.Link+"\n"))
		}
		return drain(content)
	}

	if !t.cfg.AllowSecrets && looksSecret(n.Path) {
		t.secrets = append(t.secrets, n.Path)
		return drain(content)
	}
	if n.Size > t.cfg.MaxFileSize && !isDatabaseDump(n.Path) {
		t.skipped++
		return drain(content)
	}

	if isDatabaseDump(n.Path) {
		return t.writeSchema(n, content)
	}

	data, err := io.ReadAll(io.LimitReader(content, t.cfg.MaxFileSize+1))
	if err != nil {
		return err
	}
	if err := drain(content); err != nil {
		return err
	}
	if int64(len(data)) > t.cfg.MaxFileSize || isBinary(data) {
		// Двоичное в git класть незачем: diff по нему нечитаем, а размер
		// репозитория растёт с каждой версией на полный объём файла.
		t.skipped++
		return nil
	}
	return t.write(n.Path, data)
}

func (t *gitTarget) Finish() error { return nil }

func (t *gitTarget) write(snapshotPath string, data []byte) error {
	// Собачка в служебных путях (/@mysql) в git-репозитории только мешает,
	// а раскладку по каталогам делает та же функция, что и обычное
	// восстановление, - включая защиту от выхода за пределы каталога.
	dst, err := restore.MapPath(t.dir, strings.ReplaceAll(snapshotPath, "@", "_"))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		return err
	}
	t.files++
	return nil
}

func isDatabaseDump(p string) bool {
	return strings.HasPrefix(p, collect.VirtualMySQL+"/") ||
		strings.HasPrefix(p, collect.VirtualPostgres+"/")
}

// writeSchema оставляет от дампа только структуру.
//
// Данные в git не нужны и вредны: они меняются постоянно и раздували бы
// репозиторий, а структура меняется редко - и именно её изменение
// («кто и когда добавил этот индекс») хочется увидеть в истории.
func (t *gitTarget) writeSchema(n *repo.Node, content io.Reader) error {
	// Дамп PostgreSQL в формате custom - двоичный, структуру из него
	// текстом не достать без pg_restore. Пропускаем, отметив факт.
	if strings.HasSuffix(n.Path, ".dump") {
		t.skipped++
		return drain(content)
	}

	var out bytes.Buffer
	br := bufio.NewReaderSize(content, 256<<10)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 && keepInSchema(line) {
			out.Write(line)
			if out.Len() > int(t.cfg.MaxFileSize)*8 {
				break // структура таких размеров в diff всё равно бесполезна
			}
		}
		if err != nil {
			break
		}
	}
	if err := drain(br); err != nil {
		return err
	}
	return t.write(strings.TrimSuffix(n.Path, ".sql")+".schema.sql", out.Bytes())
}

// keepInSchema отбрасывает строки с данными.
func keepInSchema(line []byte) bool {
	t := bytes.ToUpper(bytes.TrimLeft(line, " \t"))
	for _, skip := range [][]byte{
		[]byte("INSERT INTO"), []byte("REPLACE INTO"),
		[]byte("LOCK TABLES"), []byte("UNLOCK TABLES"),
		[]byte("/*!40000 ALTER TABLE"),
	} {
		if bytes.HasPrefix(t, skip) {
			return false
		}
	}
	// Строки с секретами в git не пускаем ни при каких условиях. Дампы
	// пользователей и ролей (@grants.sql, @globals.sql) содержат хеши
	// паролей учёток БД: `GRANT ... IDENTIFIED BY PASSWORD '*hash'`,
	// `CREATE ROLE ... PASSWORD 'SCRAM-SHA-256$...'`, `ALTER USER ...
	// IDENTIFIED BY ...`. Даже хеш - секрет: его подбирают офлайн, а для
	// mysql_native_password он и вовсе эквивалентен паролю. В неудаляемой
	// истории git, тем более на внешнем хостинге, им не место.
	if containsSecretClause(t) {
		return false
	}
	// Строки с датой дампа меняются каждый раз и давали бы diff при
	// полном отсутствии изменений в структуре.
	if bytes.Contains(t, []byte("-- DUMP COMPLETED ON")) {
		return false
	}
	return true
}

// containsSecretClause ловит объявления учёток и ролей с паролями/хешами.
// Строка уже в верхнем регистре.
func containsSecretClause(upper []byte) bool {
	for _, needle := range [][]byte{
		[]byte("IDENTIFIED BY"),   // MySQL GRANT/CREATE USER ... IDENTIFIED BY [PASSWORD]
		[]byte("IDENTIFIED WITH"), // MySQL 8 auth plugin + hash
		[]byte("SET PASSWORD"),
		// PG роли: пароль всегда идёт как литерал сразу за PASSWORD. Именно
		// «PASSWORD '», а не просто «PASSWORD», чтобы не задеть колонку с
		// именем password в CREATE TABLE.
		[]byte("PASSWORD '"),
		[]byte("PASSWORD E'"), // экранированный литерал PG
	} {
		if bytes.Contains(upper, needle) {
			return true
		}
	}
	return false
}

// isBinary - та же эвристика, что у самого git: нулевой байт в начале.
func isBinary(data []byte) bool {
	head := data
	if len(head) > 8000 {
		head = head[:8000]
	}
	return bytes.IndexByte(head, 0) >= 0
}

func drain(r io.Reader) error {
	if r == nil {
		return nil
	}
	_, err := io.Copy(io.Discard, r)
	return err
}
