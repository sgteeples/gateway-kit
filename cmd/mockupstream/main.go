// Command mockupstream starts one mock upstream per port so the example
// gateway.yaml can be exercised locally (defaults to ports 3001-3006).
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/sgteeples/gateway-kit/internal/mockupstream"
)

func main() {
	ports := flag.String("ports", "3001,3002,3003,3004,3005,3006", "comma-separated ports to listen on")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var servers []*http.Server
	for _, port := range strings.Split(*ports, ",") {
		port = strings.TrimSpace(port)
		srv := &http.Server{
			Addr:              ":" + port,
			Handler:           mockupstream.New("mock-" + port),
			ReadHeaderTimeout: 5 * time.Second,
		}
		servers = append(servers, srv)
		go func() {
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Fatalf("mock upstream on :%s: %v", port, err)
			}
		}()
		fmt.Printf("mock upstream listening on :%s\n", port)
	}

	<-ctx.Done()
	for _, srv := range servers {
		_ = srv.Shutdown(context.Background())
	}
}
