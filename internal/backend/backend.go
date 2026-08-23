// Package backend - куда физически ложится репозиторий.
//
// Реализаций две: локальный диск (репозиторий на ПК или NAS) и S3-совместимое
// хранилище. Всё остальное в autobak работает только через этот интерфейс и
// не знает, где лежат данные.
package backend

import (
	"context"
	"errors"
	"io"
	"time"
)

var (
	ErrNotFound  = errors.New("autobak: объект не найден")
	ErrReadOnly  = errors.New("autobak: хранилище открыто только на чтение")
	ErrNoDelete  = errors.New("autobak: у этого доступа нет права удаления")
	ErrExists    = errors.New("autobak: объект уже существует")
	ErrTruncated = errors.New("autobak: объект короче ожидаемого")
)

type FileInfo struct {
	Name    string
	Size    int64
	ModTime time.Time
}

// Caps описывает, что этому подключению разрешено.
//
// Агент на сервере получает ключи без права удаления. Это часть защиты, но
// не вся: код на нашей стороне не может помешать тому, у кого на руках сами
// ключи от хранилища (а в push-режиме они лежат на сервере), обратиться к
// S3 напрямую в обход autobak. Настоящая неизменяемость прошлых бэкапов
// достигается только на стороне хранилища - Object Lock и версионирование.
// Наши проверки закрывают ошибки в собственном коде и защищают локальный
// репозиторий, но не заменяют WORM в облаке.
type Caps struct {
	CanWrite  bool
	CanDelete bool
}

type Backend interface {
	// Location - человекочитаемое описание для логов и интерфейса.
	// Не должно содержать секретов.
	Location() string
	Caps() Caps

	Get(ctx context.Context, name string) (io.ReadCloser, error)
	// GetRange читает кусок объекта. Благодаря этому один чанк достаётся из
	// 16-мегабайтного пака в S3 без скачивания пака целиком.
	GetRange(ctx context.Context, name string, off, length int64) (io.ReadCloser, error)
	// Put записывает объект целиком. size обязателен и должен быть точным:
	// частично записанный объект недопустим, поэтому запись атомарна -
	// объект либо появляется полностью, либо не появляется вовсе.
	// Put перезаписывает существующий объект - применять только к
	// изменяемым (блокировки, config при первичной настройке).
	Put(ctx context.Context, name string, r io.Reader, size int64) error
	// PutNew записывает объект, только если его ещё нет, иначе возвращает
	// ErrExists. Для неизменяемых объектов (паки, индексы, снимки, ключи):
	// их имена привязаны к содержимому или уникальны, поэтому «объект уже
	// есть» означает «ровно те же байты уже записаны» - повтор после сбоя
	// сети безопасен, а перезапись мусором чужого пака - нет.
	PutNew(ctx context.Context, name string, r io.Reader, size int64) error
	Stat(ctx context.Context, name string) (FileInfo, error)
	List(ctx context.Context, prefix string, fn func(FileInfo) error) error
	Delete(ctx context.Context, name string) error
	Close() error
}

// ReadAll читает объект целиком с ограничением сверху: битый или враждебный
// репозиторий не должен уметь заставить нас выделить гигабайт под «индекс».
func ReadAll(ctx context.Context, b Backend, name string, limit int64) ([]byte, error) {
	rc, err := b.Get(ctx, name)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("autobak: объект " + name + " больше допустимого размера")
	}
	return data, nil
}
