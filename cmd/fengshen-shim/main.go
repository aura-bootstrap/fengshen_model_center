// Command fengshen-shim adapts one legacy local component to the center's
// loopback HTTP/WS contract. It is started by the center with a fixed mode,
// target; it never accepts commands or URLs.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"time"

	"github.com/aura-bootstrap/fengshen_model_center/internal/shim"
)

func main() {
	mode := flag.String("mode", "", "whisper-stream, whisper-batch, timbre, haar or llama")
	listen := flag.String("listen", "", "loopback listen address")
	target := flag.String("target", "", "absolute path to the legacy component executable")
	workDir := flag.String("work-dir", "", "absolute scratch directory")
	frontal := flag.String("haar-frontal", "", "absolute path to the frontal Haar XML")
	profile := flag.String("haar-profile", "", "absolute path to the profile Haar XML")
	flag.Parse()

	host, _, err := net.SplitHostPort(*listen)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		log.Fatal("listen must be a loopback host:port")
	}
	if *workDir == "" {
		log.Fatal("work-dir is required")
	}
	server, err := shim.New(shim.Config{
		Mode:       *mode,
		Target:     *target,
		WorkRoot:   *workDir,
		FrontalXML: *frontal,
		ProfileXML: *profile,
	})
	if err != nil {
		log.Fatal(err)
	}
	defer server.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	httpServer := &http.Server{
		Addr:              *listen,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdown)
	}()
	fmt.Fprintln(os.Stderr, "fengshen-shim ready")
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
