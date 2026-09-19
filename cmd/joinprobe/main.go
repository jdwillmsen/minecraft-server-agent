// Command joinprobe answers whether a client can still start a session with
// the Bedrock server, on a schedule, as a metric.
//
// It exists because every check this cluster ran during the 2026-09-15 outage
// passed for its entire duration while nobody could join: the server-list ping
// answered, mc-monitor reported players online, and all three kubelet probes
// were green. The server was a version behind its clients and refused them
// before login — a step past everything being checked.
//
// This runs as a Deployment beside the server rather than as a CronJob: the
// check is cheap, wants to be frequent, and publishing over HTTP means no
// ConfigMap, no exporter and no staleness contract to maintain.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/jdwillmsen/minecraft-server-agent/internal/joinprobe"
	"github.com/jdwillmsen/minecraft-server-agent/pkg/logging"
)

func main() {
	address := flag.String("address", "", "host:port of the Bedrock server to probe (required)")
	interval := flag.Duration("interval", time.Minute, "how often to probe")
	protocol := flag.Int("protocol", 0, "protocol number to announce; 0 uses whatever the server advertises")
	listen := flag.String("listen", ":9103", "address for /metrics and /healthz")
	level := flag.String("log-level", "info", "log level")
	flag.Parse()

	log := logging.New(*level)
	if *address == "" {
		log.Error("missing_address", logging.Fields{"detail": "-address is required"})
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	// Liveness only. There is deliberately no readiness gate on the probe
	// having succeeded: a server nobody can join must leave this pod Ready and
	// publishing zeros, because a pod that goes NotReady stops being scraped,
	// and an alert cannot fire on a series nobody is collecting.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintln(w, "ok")
	})

	server := &http.Server{
		Addr:              *listen,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()

	log.Info("probe_started", logging.Fields{
		"address":  *address,
		"interval": interval.String(),
		"protocol": *protocol,
		"listen":   *listen,
	})

	done := make(chan struct{})
	go func() {
		joinprobe.Run(ctx, *address, int32(*protocol), *interval, log)
		close(done)
	}()

	select {
	case err := <-serveErr:
		// The metrics endpoint dying is fatal: the probe would carry on
		// measuring into a void, and the silence reads as a healthy server
		// nobody happens to be scraping.
		log.Error("metrics_server_failed", logging.Fields{"error": err.Error()})
		stop()
		<-done
		os.Exit(1)
	case <-done:
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdownCtx)
	log.Info("probe_stopped", nil)
}
