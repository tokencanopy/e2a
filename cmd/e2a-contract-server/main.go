package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/tokencanopy/e2a/internal/testutil"
)

func main() {
	var envFile string
	flag.StringVar(&envFile, "env-file", "", "path to write E2A_TEST_* env vars")
	flag.Parse()

	ctx := context.Background()
	srv, err := testutil.StartContractServer(ctx, testutil.TestDBURL())
	if err != nil {
		log.Fatalf("start contract server: %v", err)
	}

	if err := waitForHealth(srv.BaseURL); err != nil {
		log.Fatalf("contract server health check failed: %v", err)
	}

	// E2A_TEST_CAPPED_API_KEY authenticates the secondary account seeded with
	// testutil.CappedLimits. Scenarios that assert quota enforcement run as
	// that account; CI sources this file with `set -a`, so the runners pick it
	// up with no workflow change.
	envContent := fmt.Sprintf(
		"E2A_TEST_BASE_URL=%s\nE2A_TEST_API_KEY=%s\nE2A_TEST_CAPPED_API_KEY=%s\n",
		srv.BaseURL, srv.APIKey, srv.CappedAPIKey,
	)
	if envFile != "" {
		if err := os.WriteFile(envFile, []byte(envContent), 0o600); err != nil {
			log.Fatalf("write env file: %v", err)
		}
	} else {
		fmt.Print(envContent)
	}

	log.Printf("contract server ready: %s", srv.BaseURL)

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	<-sigs

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Close(shutdownCtx); err != nil {
		log.Fatalf("shutdown contract server: %v", err)
	}
}

func waitForHealth(baseURL string) error {
	client := &http.Client{Timeout: 2 * time.Second}
	var lastErr error
	for range 50 {
		resp, err := client.Get(baseURL + "/api/health")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			lastErr = fmt.Errorf("unexpected status %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		time.Sleep(100 * time.Millisecond)
	}
	return lastErr
}
