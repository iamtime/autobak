package backend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Local - репозиторий на обычной файловой системе: диск ПК, внешний HDD,
// смонтированная сетевая шара.
type Local struct {
	root string
	caps Caps
}

func OpenLocal(root string, caps Caps) (*Local, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("autobak: путь репозитория: %w", err)
	}
	if caps.CanWrite {
		if err := os.MkdirAll(abs, 0o700); err != nil {
			return nil, fmt.Errorf("autobak: не удалось создать каталог репозитория: %w", err)
		}
	} else if st, err := os.Stat(abs); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("autobak: %s не является каталогом репозитория", abs)
	}
	return &Local{root: abs, caps: caps}, nil
}

func (l *Local) Location() string { return l.root }
func (l *Local) Caps() Caps       { return l.caps }
func (l *Local) Close() error     { return nil }

// path переводит имя объекта в путь и не даёт выйти за пределы репозитория:
// имена приходят в том числе из метаданных репозитория, а им доверять нельзя.
func (l *Local) path(name string) (string, error) {
	if name == "" || strings.Contains(name, "\\") || filepath.IsAbs(name) {
		return "", fmt.Errorf("autobak: недопустимое имя объекта %q", name)
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("autobak: недопустимое имя объекта %q", name)
		}
	}
	return filepath.Join(l.root, filepath.FromSlash(name)), nil
}

func mapErr(err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return ErrNotFound
	}
	return err
}

func (l *Local) Get(ctx context.Context, name string) (io.ReadCloser, error) {
	p, err := l.path(name)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, mapErr(err)
	}
	return f, nil
}

func (l *Local) GetRange(ctx context.Context, name string, off, length int64) (io.ReadCloser, error) {
	p, err := l.path(name)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, mapErr(err)
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}
	return struct {
		io.Reader
		io.Closer
	}{io.LimitReader(f, length), f}, nil
}

// Put пишет через временный файл и переименование: прерванная запись
// оставляет мусорный tmp-файл, но никогда - полуобрезанный объект,
// который потом молча вернётся вместо данных.
func (l *Local) Put(ctx context.Context, name string, r io.Reader, size int64) error {
	return l.put(ctx, name, r, size, false)
}

// PutNew пишет объект, только если его ещё нет.
func (l *Local) PutNew(ctx context.Context, name string, r io.Reader, size int64) error {
	return l.put(ctx, name, r, size, true)
}

func (l *Local) put(ctx context.Context, name string, r io.Reader, size int64, createOnly bool) error {
	if !l.caps.CanWrite {
		return ErrReadOnly
	}
	p, err := l.path(name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName)
	}()

	n, err := io.Copy(tmp, r)
	if err != nil {
		return err
	}
	if size >= 0 && n != size {
		return fmt.Errorf("autobak: %s: записано %d байт вместо %d", name, n, size)
	}
	// Без Sync переименование может пережить содержимое при потере питания.
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	if createOnly {
		// Link атомарен и падает с EEXIST, если объект уже есть: так
		// перезапись неизменяемого объекта исключена без гонки
		// «проверил-потом-записал». Rename бы молча затёр существующий.
		if err := os.Link(tmpName, p); err != nil {
			if errors.Is(err, os.ErrExist) {
				return ErrExists
			}
			return err
		}
		// Каталог с новым именем тоже стоит сбросить на диск: без этого
		// созданный объект может исчезнуть при потере питания, а
		// ссылающийся на него индекс - уцелеть.
		syncDir(filepath.Dir(p))
		return nil
	}
	if err := os.Rename(tmpName, p); err != nil {
		return err
	}
	syncDir(filepath.Dir(p))
	return nil
}

// syncDir сбрасывает запись каталога на диск, чтобы появление или
// переименование файла пережило потерю питания. Ошибка не критична:
// на некоторых ФС каталог открыть на запись нельзя, и это нормально.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}

func (l *Local) Stat(ctx context.Context, name string) (FileInfo, error) {
	p, err := l.path(name)
	if err != nil {
		return FileInfo{}, err
	}
	st, err := os.Stat(p)
	if err != nil {
		return FileInfo{}, mapErr(err)
	}
	return FileInfo{Name: name, Size: st.Size(), ModTime: st.ModTime()}, nil
}

func (l *Local) List(ctx context.Context, prefix string, fn func(FileInfo) error) error {
	base := l.root
	if i := strings.LastIndex(prefix, "/"); i >= 0 {
		sub, err := l.path(prefix[:i])
		if err != nil {
			return err
		}
		base = sub
	}
	err := filepath.WalkDir(base, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil // каталога ещё нет - это пустой список, а не сбой
			}
			return err
		}
		if d.IsDir() {
			return ctx.Err()
		}
		rel, err := filepath.Rel(l.root, p)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(rel)
		if !strings.HasPrefix(name, prefix) || strings.HasPrefix(d.Name(), ".tmp-") {
			return nil
		}
		st, err := d.Info()
		if err != nil {
			return mapErr(err)
		}
		return fn(FileInfo{Name: name, Size: st.Size(), ModTime: st.ModTime()})
	})
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (l *Local) Delete(ctx context.Context, name string) error {
	if !l.caps.CanDelete {
		return ErrNoDelete
	}
	p, err := l.path(name)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// StronglyConsistent сообщает, что листинг сразу отражает записи. Для
// локальной файловой системы это так, поэтому блокировкам не нужна пауза
// на «устаканивание» видимости, которая нужна части S3-совместимых хранилищ.
func (l *Local) StronglyConsistent() bool { return true }
