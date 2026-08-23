package main

import (
	"context"
	"strings"

	"github.com/iamtime/autobak/internal/uiapi"
	"github.com/iamtime/autobak/internal/webui"
	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/windows"
	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// UI - оконная обёртка над общим ядром.
//
// Всё содержательное живёт в uiapi и одинаково для окна и для веба.
// Здесь остаётся только то, чего в вебе быть не может: диалоги выбора
// файлов и открытие каталога в проводнике.
type UI struct {
	*uiapi.API
	ctx context.Context
}

func runUI(_ context.Context) error {
	a, err := openApp()
	if err != nil {
		return err
	}
	uiapi.Version = Version

	ui := &UI{API: uiapi.New(a, nil)}
	return wails.Run(&options.App{
		Title:       "AutoBak",
		Width:       1200,
		Height:      780,
		MinWidth:    900,
		MinHeight:   600,
		AssetServer: &assetserver.Options{Assets: webui.Assets(), Handler: nil},
		OnStartup: func(ctx context.Context) {
			ui.ctx = ctx
			// События уходят в окно только после его создания: до этого
			// момента отправлять их некуда.
			ui.API.SetEmit(func(name string, data any) {
				wruntime.EventsEmit(ctx, name, data)
			})
		},
		Bind:             []any{ui},
		Windows:          &windows.Options{WebviewIsTransparent: false, WindowIsTranslucent: false},
		BackgroundColour: &options.RGBA{R: 17, G: 18, B: 22, A: 255},
	})
}

// --- Диалоги ОС -----------------------------------------------------------
//
// В вебе этих методов нет: сервер не может открыть окно выбора файла на
// чужом компьютере. Там соответствующие поля заполняются вручную.

func (u *UI) PickFolder(title string) (string, error) {
	return wruntime.OpenDirectoryDialog(u.ctx, wruntime.OpenDialogOptions{Title: title})
}

func (u *UI) PickFile(title string) (string, error) {
	return wruntime.OpenFileDialog(u.ctx, wruntime.OpenDialogOptions{Title: title})
}

func (u *UI) OpenFolder(dir string) error {
	wruntime.BrowserOpenURL(u.ctx, "file:///"+strings.ReplaceAll(dir, "\\", "/"))
	return nil
}
