// Package secretstore хранит секреты десктопа: пароли репозиториев,
// ключи от S3, токены git.
//
// Ничего из этого не должно лежать рядом с конфигурацией открытым
// текстом. На Windows шифрование делает сама система (DPAPI): данные
// привязаны к учётной записи пользователя, и скопированный на другую
// машину файл там не расшифруется. Это заметно лучше, чем собственный
// пароль на пароли, который пользователь всё равно сохранит в браузере.
package secretstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

var ErrNotFound = errors.New("autobak: секрет не найден")

type Store struct {
	mu     sync.RWMutex
	path   string
	values map[string]string
	loaded bool
}

func New(path string) *Store {
	return &Store{path: path, values: map[string]string{}}
}

func (s *Store) load() error {
	if s.loaded {
		return nil
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			s.loaded = true
			return nil
		}
		return err
	}
	plain, err := unprotect(raw)
	if err != nil {
		return fmt.Errorf("autobak: хранилище секретов не расшифровывается: %w", err)
	}
	if err := json.Unmarshal(plain, &s.values); err != nil {
		return fmt.Errorf("autobak: хранилище секретов повреждено: %w", err)
	}
	s.loaded = true
	return nil
}

func (s *Store) Get(key string) (string, error) {
	s.mu.RLock()
	if s.loaded {
		v, ok := s.values[key]
		s.mu.RUnlock()
		if !ok {
			return "", ErrNotFound
		}
		return v, nil
	}
	s.mu.RUnlock()

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return "", err
	}
	v, ok := s.values[key]
	if !ok {
		return "", ErrNotFound
	}
	return v, nil
}

func (s *Store) Set(key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return err
	}
	s.values[key] = value
	return s.save()
}

func (s *Store) Delete(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return err
	}
	delete(s.values, key)
	return s.save()
}

// Has не расшифровывает значение - удобно, чтобы интерфейс мог показать
// «пароль сохранён», не доставая сам пароль.
func (s *Store) Has(key string) bool {
	_, err := s.Get(key)
	return err == nil
}

func (s *Store) save() error {
	raw, err := json.Marshal(s.values)
	if err != nil {
		return err
	}
	sealed, err := protect(raw)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	// Через временный файл: оборванная запись не должна оставить
	// повреждённое хранилище, из которого потом не достать ни один пароль.
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, sealed, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
