// Command mockupstream runs a fake OpenAI-compatible provider for local dev,
// load tests and chaos tests.
package main

import (
	"flag"
	"log"
	"net/http"
	"time"

	"github.com/krishbansalofficial/tollgate/internal/mockupstream"
)

func main() {
	addr := flag.String("addr", ":8081", "listen address")
	var cfg mockupstream.Config
	flag.DurationVar(&cfg.Latency, "latency", 50*time.Millisecond, "base latency")
	flag.DurationVar(&cfg.Jitter, "jitter", 0, "uniform extra latency in [0, jitter)")
	flag.Float64Var(&cfg.ErrorRate, "error-rate", 0, "fraction of requests returning 500")
	flag.Float64Var(&cfg.RateLimitRate, "429-rate", 0, "fraction of requests returning 429")
	flag.IntVar(&cfg.StreamChunks, "stream-chunks", 10, "content chunks per streamed reply")
	flag.DurationVar(&cfg.ChunkDelay, "chunk-delay", 0, "delay before each streamed chunk")
	flag.IntVar(&cfg.StreamFailAfter, "stream-fail-after", 0, "abort streams after N content chunks (0 = never)")
	flag.Parse()

	log.Printf("mockupstream listening on %s %+v", *addr, cfg)
	srv := &http.Server{Addr: *addr, Handler: mockupstream.New(cfg).Handler(), ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(srv.ListenAndServe())
}
