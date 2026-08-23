package app

import (
	"context"
	"sync"
	"testing"
	"time"
)

// Веб-сервер обслуживает запросы параллельно: чтение состояния идёт
// одновременно с бэкапом, дописывающим в конфигурацию свой итог.
// Тест воспроизводит это и ловится детектором гонок (go test -race).
func TestConcurrentConfigAccess(t *testing.T) {
	a, err := OpenAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cr := &Repo{Name: "тест", Kind: RepoLocal, Path: t.TempDir()}
	if _, err := a.AddRepo(context.Background(), cr, "", "пароль-репозитория"); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		s := &Server{Name: string(rune('a' + i)), RepoID: cr.ID, Mode: ModePull}
		if err := a.AddServer(s); err != nil {
			t.Fatal(err)
		}
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Пишущие: так конфигурацию меняет завершившийся бэкап.
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = a.Update(func(c *Config) error {
					for _, s := range c.Servers {
						s.Last = LastRun{Time: time.Now(), OK: true, Bytes: 42}
					}
					return nil
				})
			}
		}()
	}

	// Читающие: так состояние запрашивает интерфейс.
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				a.Read(func(c *Config) {
					for _, s := range c.Servers {
						_ = s.Last.Status()
						_ = s.Schedule.Describe()
					}
					for _, r := range c.Repos {
						_ = r.Location()
					}
				})
				_ = a.DrillDue()
				_, _ = a.ServerForRepo(cr.ID)
			}
		}()
	}

	time.Sleep(300 * time.Millisecond)
	close(stop)
	wg.Wait()
}
