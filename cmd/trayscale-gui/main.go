// Command trayscale-gui is a native Windows GUI edition of Trayscale. It renders
// the upstream libadwaita window's layout (machine sidebar + detail pane) using
// Gio, a pure-Go toolkit, while reusing Trayscale's tsutil core.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"

	"deedles.dev/trayscale/internal/winui"
	"gioui.org/app"
)

func setupLogging() {
	dir, err := os.UserConfigDir()
	if err != nil {
		return
	}
	dir = filepath.Join(dir, "Trayscale")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, "trayscale-gui.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(f, &slog.HandlerOptions{Level: slog.LevelInfo})))
}

func main() {
	setupLogging()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	slog.Info("trayscale-gui starting")
	go func() {
		if err := winui.Run(ctx); err != nil {
			slog.Error("window exited with error", "err", err)
		}
		cancel()
		os.Exit(0)
	}()

	// Gio requires its event loop to run on the main goroutine.
	app.Main()
}
