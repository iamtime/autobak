# Сборка AutoBak на macOS и Linux. Для Windows есть build.ps1.
#
#   make            собрать всё в ./dist
#   make test       модульные тесты
#   make desktop    оконное приложение для этой машины (нужен Wails)
#   make web        сервер с веб-интерфейсом
#   make agent      агент для сервера (linux/amd64 и linux/arm64)

VERSION ?= 0.1.0
DIST    ?= dist
LDFLAGS  = -s -w -X main.Version=$(VERSION)
GO      ?= go

# Хост-платформа для сборки того, что запускается здесь же.
HOST_OS   := $(shell $(GO) env GOOS)
HOST_ARCH := $(shell $(GO) env GOARCH)

.PHONY: all agent web desktop cli test vet fmt clean docker-web integration

all: agent web cli

$(DIST):
	@mkdir -p $(DIST)

# Агент собирается статически: он должен запускаться на любом Linux без
# установленных библиотек, включая Alpine с musl и старые CentOS.
agent: | $(DIST)
	@for arch in amd64 arm64; do \
		echo "агент linux/$$arch"; \
		CGO_ENABLED=0 GOOS=linux GOARCH=$$arch \
			$(GO) build -trimpath -ldflags "$(LDFLAGS)" \
			-o $(DIST)/autobak-agent-linux-$$arch ./cmd/autobak-agent || exit 1; \
	done

web: | $(DIST)
	@for arch in amd64 arm64; do \
		echo "веб linux/$$arch"; \
		CGO_ENABLED=0 GOOS=linux GOARCH=$$arch \
			$(GO) build -trimpath -ldflags "$(LDFLAGS)" \
			-o $(DIST)/autobak-web-linux-$$arch ./cmd/autobak-web || exit 1; \
	done

# Командная строка для этой машины: то же ядро, что и в окне.
cli: | $(DIST)
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" \
		-o $(DIST)/autobak ./cmd/autobak

# Окно требует системного webview, а значит CGO и заголовочных файлов:
#   macOS  - Xcode Command Line Tools
#   Linux  - libgtk-3-dev и libwebkit2gtk-4.1-dev (или 4.0)
desktop: | $(DIST)
	CGO_ENABLED=1 $(GO) build -trimpath -tags "desktop,production" \
		-ldflags "$(LDFLAGS)" -o $(DIST)/autobak-$(HOST_OS)-$(HOST_ARCH) ./cmd/autobak

test:
	$(GO) test ./... -timeout 600s

vet:
	$(GO) vet ./...
	@test -z "$$(gofmt -l . | grep -v '^vendor/')" || { echo "не отформатировано:"; gofmt -l . | grep -v '^vendor/'; exit 1; }

docker-web:
	docker build -f deploy/web/Dockerfile -t autobak-web:$(VERSION) \
		--build-arg VERSION=$(VERSION) .

clean:
	rm -rf $(DIST)

# Интеграционные тесты идут в Linux-контейнере: на macOS и Windows нет ни
# настоящего sshd, ни владельцев файлов, ни xattr.
integration:
	sh test/integration.sh
