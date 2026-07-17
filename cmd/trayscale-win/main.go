// Command trayscale-win is a native Windows system-tray front-end for
// Trayscale. It reuses Trayscale's tsutil core (Tailscale LocalAPI) but renders
// its UI as a Windows tray icon + menu instead of the Linux-only GTK4 window.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"

	"deedles.dev/trayscale/internal/wintray"
)

func setupLogging() {
	// When built with -H=windowsgui there is no console, so optionally mirror
	// logs to a file for troubleshooting.
	dir, err := os.UserConfigDir()
	if err != nil {
		return
	}
	dir = filepath.Join(dir, "Trayscale")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, "trayscale.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(f, &slog.HandlerOptions{Level: slog.LevelInfo})))
}

func main() {
	setupLogging()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	slog.Info("trayscale-win starting")
	wintray.Run(ctx)
	slog.Info("trayscale-win exited")
}
