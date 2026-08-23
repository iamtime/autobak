package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Подготовка конфигурации для push-режима.
//
// В этом режиме агент пишет в хранилище сам, а значит ему нужны две
// вещи, которых на сервере нет: ключ репозитория и доступ к хранилищу.
// Собирать их вручную - верный способ ошибиться в одном символе и
// узнать об этом через месяц, когда бэкапы понадобятся.
//
// Файлы пишутся с правами 0600 и содержат секреты в открытом виде:
// иначе агент не сможет их прочитать. Это осознанная цена push-режима,
// и она указана в его описании: захватив сервер, злоумышленник получит
// доступ к бэкапам этого сервера. Именно поэтому ключи у каждого сервера
// свои, а праву на удаление в хранилище агент не обучен.

type AgentConfigResult struct {
	Dir      string   `json:"dir"`
	Files    []string `json:"files"`
	Warnings []string `json:"warnings,omitempty"`
	// Command - что выполнить на сервере после копирования файлов.
	Command string `json:"command"`
}

// WriteAgentConfig готовит config.json и key для указанного сервера.
func (a *App) WriteAgentConfig(ctx context.Context, serverID, dir string) (*AgentConfigResult, error) {
	s, err := a.cfg.Server(serverID)
	if err != nil {
		return nil, err
	}
	if len(s.Plan.Enabled()) == 0 {
		return nil, errors.New("autobak: у сервера нет плана - сначала обследуйте его")
	}
	cr, err := a.cfg.Repo(s.RepoID)
	if err != nil {
		return nil, err
	}
	// Ключ достаётся из открытого репозитория, а не из хранилища паролей:
	// так исключён случай «пароль подошёл к другому репозиторию».
	r, err := a.OpenRepo(ctx, s.RepoID)
	if err != nil {
		return nil, err
	}

	if dir == "" {
		dir = filepath.Join(a.dir, "agent", s.Name)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}

	res := &AgentConfigResult{Dir: dir}
	if s.Mode != ModePush {
		res.Warnings = append(res.Warnings,
			"сервер настроен в режиме «забирает этот компьютер». "+
				"Эти файлы нужны только для режима «сервер пишет сам».")
	}

	repoBlock, warn, err := a.repoBlockFor(cr)
	if err != nil {
		return nil, err
	}
	res.Warnings = append(res.Warnings, warn...)

	cfg := map[string]any{
		"server":   s.Name,
		"key_file": "/etc/autobak/key",
		"repo":     repoBlock,
		"plan":     s.Plan,
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, err
	}

	confPath := filepath.Join(dir, "config.json")
	keyPath := filepath.Join(dir, "key")
	if err := writePrivate(confPath, raw); err != nil {
		return nil, err
	}
	// Ключ хранится recovery-кодом, а не в двоичном виде: его можно
	// прочитать глазами, продиктовать и набрать руками при восстановлении
	// с нуля, когда под рукой нет ничего, кроме терминала.
	if err := writePrivate(keyPath, []byte(r.Key().RecoveryCode()+"\n")); err != nil {
		return nil, err
	}
	res.Files = []string{confPath, keyPath}

	res.Warnings = append(res.Warnings,
		"В этих файлах ключ от бэкапов и доступ к хранилищу в открытом виде. "+
			"Копируйте их только по защищённому каналу и не оставляйте копий на диске.")
	res.Command = "sudo mkdir -p /etc/autobak && sudo chmod 700 /etc/autobak && " +
		"sudo cp config.json key /etc/autobak/ && sudo chmod 600 /etc/autobak/* && " +
		"sudo sh install.sh --push"
	return res, nil
}

// repoBlockFor описывает хранилище так, как его ждёт агент.
func (a *App) repoBlockFor(cr *Repo) (map[string]any, []string, error) {
	switch cr.Kind {
	case RepoLocal:
		return map[string]any{"type": "local", "path": cr.Path}, []string{
			"Хранилище локальное: путь " + cr.Path + " должен существовать " +
				"на самом сервере. Бэкап рядом с данными бэкапом не является - " +
				"для push-режима нужен S3 или смонтированный удалённый диск.",
		}, nil
	case RepoS3:
		secret, err := a.secrets.Get(cr.secretKeyID())
		if err != nil {
			return nil, nil, fmt.Errorf("autobak: нет сохранённого ключа для %s: %w", cr.Name, err)
		}
		return map[string]any{
				"type":       "s3",
				"endpoint":   cr.Endpoint,
				"region":     cr.Region,
				"bucket":     cr.Bucket,
				"prefix":     cr.Prefix,
				"access_key": cr.AccessKey,
				"secret_key": secret,
				"path_style": cr.PathStyle,
			}, []string{
				"Ключ S3 попадёт на сервер. Выдайте ему права только на запись " +
					"и чтение, без удаления, и включите версионирование бакета: " +
					"тогда захваченный сервер не сможет стереть прошлые бэкапы.",
			}, nil
	}
	return nil, nil, fmt.Errorf("autobak: неизвестный тип хранилища %q", cr.Kind)
}

// writePrivate создаёт файл, недоступный посторонним, до записи в него
// секретов - а не после.
func writePrivate(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return err
	}
	return f.Close()
}
