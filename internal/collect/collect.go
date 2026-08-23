// Package collect - сборщики: то, что превращает содержимое сервера
// в поток узлов и байтов.
//
// Каждый сборщик отдаёт данные в Sink и ничего не знает ни о шифровании,
// ни о хранилище, ни о том, кто его вызвал. Благодаря этому один и тот же
// код работает и в push-режиме (агент пишет прямо в S3), и в pull-режиме
// (агент отдаёт поток десктопу по SSH).
package collect

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/iamtime/autobak/internal/plan"
	"github.com/iamtime/autobak/internal/repo"
)

// Sink принимает найденное.
//
// File обязан прочитать r до конца до возврата: сборщики отдают содержимое
// потоком (вывод mysqldump, tar из тома docker), и перемотать его нельзя.
type Sink interface {
	Meta(n *repo.Node) error
	File(n *repo.Node, r io.Reader) error
	Logf(level, format string, args ...any)
	Progress(path string, bytes int64)
}

type Collector interface {
	Kind() plan.Kind
	Name() string
	// Collect возвращает метаданные модуля для манифеста снимка.
	Collect(ctx context.Context, s Sink) (map[string]any, error)
}

// New собирает сборщик по описанию из плана.
func New(m plan.Module, global []string) (Collector, error) {
	switch m.Kind {
	case plan.KindFiles, plan.KindConfigs, plan.KindHestia:
		return newFiles(m, global), nil
	case plan.KindMySQL:
		return newMySQL(m), nil
	case plan.KindPostgres:
		return newPostgres(m), nil
	case plan.KindDocker:
		return newDocker(m), nil
	case plan.KindK8s:
		return newK8s(m), nil
	}
	return nil, fmt.Errorf("autobak: неизвестный тип модуля %q", m.Kind)
}

// virtualNode описывает объект, которого нет на диске: дамп базы, tar тома.
//
// Такие узлы кладутся под служебные пути вида /@mysql/shop.sql, чтобы при
// восстановлении их нельзя было спутать с настоящими файлами сервера и
// случайно вывалить дамп в корень файловой системы.
func virtualNode(path string, mod string) *repo.Node {
	return &repo.Node{
		Path: path, Type: repo.NodeFile, Module: mod,
		Mode: 0o600, User: "root", Group: "root",
	}
}

const (
	VirtualMySQL    = "/@mysql"
	VirtualPostgres = "/@postgres"
	VirtualDocker   = "/@docker"
)

// lookPath ищет утилиту и даёт понятную ошибку вместо "exec: not found".
func lookPath(name, hint string) (string, error) {
	p, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("autobak: на сервере не найден %s - %s", name, hint)
	}
	return p, nil
}

// trimLines превращает вывод утилиты в список непустых строк.
func trimLines(out []byte) []string {
	var res []string
	for _, l := range strings.Split(string(out), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			res = append(res, l)
		}
	}
	return res
}

// stderrTail оставляет от диагностики хвост: полный stderr от mysqldump на
// сломанной таблице занимает мегабайты и в интерфейсе бесполезен.
func stderrTail(b []byte, max int) string {
	s := strings.TrimSpace(string(b))
	if len(s) <= max {
		return s
	}
	return "..." + s[len(s)-max:]
}

// safeDBName отклоняет имена баз, которыми можно провести инъекцию опции
// в mysqldump/pg_dump.
//
// Имя базы попадает в командную строку отдельным аргументом. Утилиты
// разбирают любой аргумент, начинающийся с "-", как опцию независимо от
// позиции: база с именем "--file=/etc/cron.d/pwn" заставила бы pg_dump
// (работающий от root) писать по произвольному пути. Имя контролирует
// тот, у кого есть право CREATE DATABASE на сервере, - потенциально
// недоверенный арендатор. Настоящих баз с такими именами не бывает.
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
