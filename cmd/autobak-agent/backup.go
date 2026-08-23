package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/iamtime/autobak/internal/backend"
	"github.com/iamtime/autobak/internal/engine"
	"github.com/iamtime/autobak/internal/plan"
	"github.com/iamtime/autobak/internal/repo"
)

// agentConfig - конфигурация push-режима: агент сам, по таймеру, пишет
// бэкап в хранилище. Лежит в /etc/autobak/config.json.
type agentConfig struct {
	Server  string     `json:"server"`
	KeyFile string     `json:"key_file"`
	Repo    repoConfig `json:"repo"`
	Plan    plan.Plan  `json:"plan"`
}

type repoConfig struct {
	// Type: s3 или local. local осмыслен только для смонтированного
	// удалённого диска: держать бэкап на том же сервере - не бэкап.
	Type string `json:"type"`
	Path string `json:"path,omitempty"`

	Endpoint  string `json:"endpoint,omitempty"`
	Region    string `json:"region,omitempty"`
	Bucket    string `json:"bucket,omitempty"`
	Prefix    string `json:"prefix,omitempty"`
	AccessKey string `json:"access_key,omitempty"`
	SecretKey string `json:"secret_key,omitempty"`
	PathStyle bool   `json:"path_style,omitempty"`
}

func cmdBackup(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	cfgPath := fs.String("config", "/etc/autobak/config.json", "файл конфигурации")
	dryRun := fs.Bool("dry-run", false, "только проверить настройки и доступ к хранилищу")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := loadAgentConfig(*cfgPath)
	if err != nil {
		return err
	}
	key, err := loadKeyFile(cfg.KeyFile)
	if err != nil {
		return err
	}

	be, err := openBackend(cfg.Repo)
	if err != nil {
		return err
	}
	defer be.Close()

	r, err := repo.OpenWithKey(ctx, be, key)
	if err != nil {
		return err
	}
	if *dryRun {
		fmt.Printf("настройки в порядке: репозиторий %s, чанков в индексе %d, план - %s\n",
			be.Location(), r.Index().Count(), cfg.Plan.Describe())
		return nil
	}

	applyPriority(&cfg.Plan)
	snap, err := engine.Backup(ctx, r, engine.Options{
		Plan: &cfg.Plan, Server: cfg.Server, Agent: Version,
		Log: logToStderr,
	})
	if err != nil {
		return err
	}

	fmt.Printf("снимок %s: файлов %d, данных %s, новых %s, за %s\n",
		snap.ID, snap.Stats.Files,
		repo.HumanBytes(snap.Stats.BytesTotal),
		repo.HumanBytes(snap.Stats.BytesStored),
		fmtDuration(snap.Stats.DurationMS))

	if !snap.Complete() {
		// Ненулевой код возврата нужен, чтобы systemd пометил задание
		// провалившимся и это стало видно в мониторинге, а не потерялось
		// в журнале.
		for _, m := range snap.Failed() {
			fmt.Fprintf(os.Stderr, "модуль %q завершился с ошибкой: %s\n", m.Name, m.Err)
		}
		return errors.New("autobak: снимок сохранён, но неполон")
	}
	return nil
}

func fmtDuration(ms int64) string {
	switch {
	case ms < 1000:
		return fmt.Sprintf("%d мс", ms)
	case ms < 60000:
		return fmt.Sprintf("%.1f с", float64(ms)/1000)
	}
	return fmt.Sprintf("%d мин %d с", ms/60000, (ms%60000)/1000)
}

// loadAgentConfig читает конфигурацию и проверяет её права.
//
// В файле лежат ключи от хранилища. Если он читается кем угодно, любой
// пользователь сервера получает доступ к бэкапам - проверка обязательна
// и намеренно отказывает, а не предупреждает.
func loadAgentConfig(path string) (*agentConfig, error) {
	if err := checkPrivateFile(path); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("autobak: не прочитать %s: %w", path, err)
	}
	var cfg agentConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("autobak: %s повреждён: %w", path, err)
	}
	if cfg.Server == "" {
		return nil, errors.New("autobak: в конфигурации не указано имя сервера")
	}
	if cfg.KeyFile == "" {
		cfg.KeyFile = "/etc/autobak/key"
	}
	if err := cfg.Plan.Validate(); err != nil {
		return nil, err
	}
	// Серверное ограничение действует и здесь: если backup вызвали через
	// ограниченный ключ (serve --backup-only --allow=...), план из конфига
	// тоже не должен выходить за разрешённые каталоги. При локальном
	// запуске по расписанию serveAllow пуст - ограничений нет.
	if err := cfg.Plan.CheckAllowed(serveAllow); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// loadKeyFile читает master key репозитория.
//
// Формат - recovery-код: его можно прочитать глазами, продиктовать и
// набрать руками при восстановлении с нуля, в отличие от бинарного файла.
func loadKeyFile(path string) (*repo.MasterKey, error) {
	if err := checkPrivateFile(path); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("autobak: не прочитать ключ %s: %w", path, err)
	}
	key, err := repo.ParseRecoveryCode(strings.TrimSpace(string(data)))
	if err != nil {
		return nil, fmt.Errorf("autobak: файл ключа %s непригоден: %w", path, err)
	}
	return key, nil
}

func openBackend(c repoConfig) (backend.Backend, error) {
	// Права на удаление агент не получает никогда. Даже полностью
	// захваченный сервер не может стереть или перезаписать прошлые
	// бэкапы - именно это отличает бэкап от копии, которую шифровальщик
	// уничтожит вместе с оригиналом.
	caps := backend.Caps{CanWrite: true, CanDelete: false}
	switch c.Type {
	case "s3":
		return backend.OpenS3(backend.S3Config{
			Endpoint: c.Endpoint, Region: c.Region, Bucket: c.Bucket,
			Prefix: c.Prefix, AccessKey: c.AccessKey, SecretKey: c.SecretKey,
			PathStyle: c.PathStyle, Caps: caps,
		})
	case "local", "":
		if c.Path == "" {
			return nil, errors.New("autobak: не указан путь к репозиторию")
		}
		return backend.OpenLocal(c.Path, caps)
	}
	return nil, fmt.Errorf("autobak: неизвестный тип хранилища %q", c.Type)
}

func cmdSelftest(ctx context.Context) error {
	type check struct {
		name string
		ok   bool
		note string
	}
	var checks []check

	add := func(name string, ok bool, note string) {
		checks = append(checks, check{name, ok, note})
	}

	add("запуск от root", os.Geteuid() == 0,
		"без root часть файлов и баз будет недоступна")

	for _, bin := range []string{"mysqldump", "mariadb-dump"} {
		if p, err := exec.LookPath(bin); err == nil {
			add("клиент MySQL", true, p)
			break
		}
	}
	if _, err := exec.LookPath("pg_dump"); err == nil {
		add("клиент PostgreSQL", true, "")
	}
	if _, err := exec.LookPath("docker"); err == nil {
		out, err := exec.CommandContext(ctx, "docker", "ps", "-q").CombinedOutput()
		add("docker", err == nil, strings.TrimSpace(string(out)))
	}
	if _, err := os.Stat("/etc/autobak/config.json"); err == nil {
		add("конфигурация push-режима", checkPrivateFile("/etc/autobak/config.json") == nil,
			"файл должен быть доступен только root (chmod 600)")
	}

	failed := 0
	for _, c := range checks {
		mark := "OK "
		if !c.ok {
			mark = "!! "
			failed++
		}
		fmt.Printf("%s %-28s %s\n", mark, c.name, c.note)
	}
	if failed > 0 {
		return fmt.Errorf("autobak: проверок не пройдено: %d", failed)
	}
	return nil
}
