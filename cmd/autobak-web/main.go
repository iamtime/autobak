// Команда autobak-web - то же приложение, но с доступом через браузер.
//
// Ставится на отдельный сервер для бэкапов: развернул контейнер, указал
// порт и пароль, зашёл браузером. Ядро то же самое, что в оконной версии,
// поэтому набор возможностей и все подтверждения перед необратимыми
// действиями совпадают до буквы.
//
// Настраивается переменными окружения - так удобнее в docker compose,
// и так пароль не остаётся в истории команд:
//
//	AUTOBAK_ADDR       где слушать (по умолчанию 127.0.0.1:8080)
//	AUTOBAK_USER       имя пользователя (по умолчанию admin)
//	AUTOBAK_PASSWORD   пароль; если не задан, будет создан и напечатан
//	AUTOBAK_DATA       каталог с настройками и секретами
//	AUTOBAK_TLS_CERT   сертификат и ключ, если TLS обслуживается сами
//	AUTOBAK_TLS_KEY
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/iamtime/autobak/internal/app"
	"github.com/iamtime/autobak/internal/uiapi"
)

var Version = "dev"

func main() {
	log.SetFlags(log.LstdFlags)

	addr := flag.String("addr", env("AUTOBAK_ADDR", "127.0.0.1:8080"), "адрес и порт")
	dataDir := flag.String("data", env("AUTOBAK_DATA", ""), "каталог с настройками")
	certFile := flag.String("tls-cert", env("AUTOBAK_TLS_CERT", ""), "файл сертификата")
	keyFile := flag.String("tls-key", env("AUTOBAK_TLS_KEY", ""), "файл ключа")
	showVersion := flag.Bool("version", false, "показать версию")
	flag.Parse()

	if *showVersion {
		fmt.Printf("autobak-web %s\n", Version)
		return
	}
	if err := run(*addr, *dataDir, *certFile, *keyFile); err != nil {
		log.Fatalf("autobak-web: %v", err)
	}
}

func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func envBool(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func run(addr, dataDir, certFile, keyFile string) error {
	app.Version = Version
	uiapi.Version = Version

	a, err := openApp(dataDir)
	if err != nil {
		return err
	}

	password := os.Getenv("AUTOBAK_PASSWORD")
	generated := false
	if password == "" {
		// Пустой пароль по умолчанию не рассматривается: такие установки
		// живут годами, а находят их сканеры за часы.
		password = randomPassword()
		generated = true
	}
	au, err := newAuth(os.Getenv("AUTOBAK_USER"), password)
	if err != nil {
		return err
	}
	// Пароль больше не нужен: дальше сравниваются только хэши.
	os.Unsetenv("AUTOBAK_PASSWORD")

	useTLS := certFile != "" && keyFile != ""
	// TLS может терминировать обратный прокси - тогда TLS у нас нет, но
	// соединение всё равно защищённое. Об этом сообщают явно, чтобы кука
	// стала Secure и включился HSTS. Заодно только в этом случае имеет
	// смысл доверять X-Forwarded-For.
	behindProxy := envBool("AUTOBAK_BEHIND_TLS_PROXY")
	trustProxy := behindProxy || envBool("AUTOBAK_TRUST_PROXY")

	api := uiapi.New(a, nil)
	srv := newServer(api, au, serverOptions{
		tls:        useTLS,
		secure:     behindProxy,
		trustProxy: trustProxy,
	})

	warnExposure(addr, useTLS || behindProxy)
	if generated {
		log.Printf("пароль не задан, создан временный:\n\n    %s\n\n"+
			"Задайте свой через AUTOBAK_PASSWORD - этот пропадёт при перезапуске.", password)
	}

	httpSrv := &http.Server{
		Addr:    addr,
		Handler: srv.routes(),
		// Заголовки читаются с ограничением по времени: иначе открытое
		// соединение, не присылающее запрос, занимает поток бесконечно.
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// Ограничения на запись нет намеренно: поток событий живёт
		// столько, сколько открыта вкладка, а восстановление большого
		// снимка идёт часами.
		ErrorLog: log.New(os.Stderr, "http: ", log.LstdFlags),
	}
	if useTLS {
		httpSrv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Расписание живёт внутри процесса: на выделенной машине для бэкапов
	// ставить рядом ещё и cron значило бы усложнить установку ровно там,
	// где вся ценность в «развернул - и работает».
	go runScheduler(ctx, api)

	errc := make(chan error, 1)
	go func() {
		scheme := "http"
		if useTLS {
			scheme = "https"
		}
		log.Printf("autobak-web %s: %s://%s (настройки в %s)", Version, scheme, addr, a.Dir())
		if useTLS {
			errc <- httpSrv.ListenAndServeTLS(certFile, keyFile)
			return
		}
		errc <- httpSrv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		log.Println("остановка...")
		// Времени на завершение немного: длинные операции живут в своих
		// горутинах и всё равно прервутся, а держать порт занятым при
		// перезапуске контейнера незачем.
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutCtx)
	}
}

func openApp(dataDir string) (*app.App, error) {
	if dataDir != "" {
		return app.OpenAt(dataDir)
	}
	return app.Open()
}

// warnExposure предупреждает о том, что интерфейс виден снаружи.
//
// Через него доступны все бэкапы и восстановление на любой сервер, то
// есть по сути root на всех подключённых машинах. Открытый наружу порт
// без TLS означает пароль открытым текстом в чужой сети.
func warnExposure(addr string, useTLS bool) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return
	}
	loopback := host == "127.0.0.1" || host == "::1" || strings.EqualFold(host, "localhost")
	if loopback {
		return
	}
	log.Printf("ВНИМАНИЕ: интерфейс слушает на %s, то есть доступен из сети.", addr)
	if !useTLS {
		log.Printf("ВНИМАНИЕ: TLS не настроен - пароль и содержимое пойдут открытым текстом.")
		log.Printf("Поставьте перед ним обратный прокси с сертификатом либо задайте")
		log.Printf("AUTOBAK_TLS_CERT и AUTOBAK_TLS_KEY.")
	}
	log.Printf("Через этот интерфейс доступно восстановление на любой сервер -")
	log.Printf("по сути это root на всех подключённых машинах.")
}
