package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	articlestore "github.com/kayushkin/article-store"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

func main() {
	settings, err := articlestore.NewSettingsRegistry(servicesettings.ProcessEnvironment())
	if err != nil {
		log.Fatalf("read settings: %v", err)
	}
	addr := settings.String(articlestore.SettingListenAddress)

	store, err := articlestore.Open(settings.String(articlestore.SettingDataDirectory), &http.Client{Timeout: articlestore.FetchTimeout})
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer store.Close()

	backfillContext, stopBackfills := context.WithCancel(context.Background())
	defer stopBackfills()
	backfiller := &articlestore.Backfiller{
		Store:           store,
		RequestInterval: settings.Duration(articlestore.SettingBackfillRequestInterval),
		IdleInterval:    30 * time.Second,
	}
	go backfiller.Run(backfillContext)
	watcher := &articlestore.Watcher{
		Store:           store,
		RequestInterval: settings.Duration(articlestore.SettingBackfillRequestInterval),
		IdleInterval:    time.Minute,
	}
	go watcher.Run(backfillContext)

	mux := http.NewServeMux()
	articlestore.RegisterHandlers(mux, store)
	articlestore.RegisterSettingsHandler(mux, settings)

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		log.Printf("article-store listening on %s (data=%s)", addr, store.DataDir())
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Println("shutting down…")
	stopBackfills()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}
