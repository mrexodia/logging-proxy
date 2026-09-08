// proxy-webui observes an existing log directory without starting or restarting
// any proxy listener. It can safely run alongside an older proxy binary.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"time"

	"github.com/mrexodia/logging-proxy/webui"
)

func main() {
	dir := flag.String("log-dir", "logs", "existing capture directory (read-only)")
	host := flag.String("host", "127.0.0.1", "loopback address to listen on")
	port := flag.Int("port", 5602, "web UI port")
	flag.Parse()
	address, err := webui.ListenAddress(*host, *port)
	if err != nil {
		log.Fatal(err)
	}
	store, err := webui.NewStore(*dir)
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	go store.Run(ctx)
	server := &http.Server{Addr: address, Handler: webui.NewHandler(store), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
		_ = server.Close()
	}()
	log.Printf("Read-only traffic inspector: http://%s (logs: %s)", address, *dir)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
