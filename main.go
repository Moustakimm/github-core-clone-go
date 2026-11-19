// main.go — Go entrypoint: the harness lifecycle, ported from the Node
// main.ts.
//
// Trial mode (MAKEFUN_ENV_URL set): bind an ephemeral port — never 8080,
// the env owns it — POST $MAKEFUN_ENV_URL/register {"port": N}, then poll
// GET $MAKEFUN_ENV_URL/done until {"done": true}, close and exit 0. A 409
// from register means another entry registered first; this process keeps
// waiting like any other. 400/405 from register are contract errors and
// end the program with exit 1.
//
// Dev mode (MAKEFUN_ENV_URL unset): listen on $PORT (default 3000) on
// 127.0.0.1 and serve forever — this is how the local SDK runner tests
// the server.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// How long one register or done request may take before it is abandoned
// and retried.
const requestTimeout = 30 * time.Second

// The first wait between retries, and the longest.
const (
	retryInitial = 250 * time.Millisecond
	retryMax     = 2 * time.Second
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "[entry] %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	envURL := strings.TrimRight(os.Getenv("MAKEFUN_ENV_URL"), "/")
	store := NewStore()
	handler := createHandler(store)

	if envURL == "" {
		// Dev mode: fixed port on loopback, serve forever.
		port := 3000
		if p := os.Getenv("PORT"); p != "" {
			if n, err := strconv.Atoi(p); err == nil {
				port = n
			}
		}
		ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
		if err != nil {
			return err
		}
		fmt.Printf("[entry] listening on port %d\n", port)
		return http.Serve(ln, handler)
	}

	// Trial mode: ephemeral port, register it, wait for done.
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		return err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	fmt.Printf("[entry] listening on port %d\n", port)
	go func() {
		_ = http.Serve(ln, handler)
	}()

	if err := register(envURL, port); err != nil {
		return err
	}
	fmt.Println("[entry] registered; waiting for the env to say done")

	waitForDone(envURL)
	_ = ln.Close()
	fmt.Println("[entry] done")
	return nil
}

// retryDelay is the wait after `attempt` failed requests: exponential
// from retryInitial, capped at retryMax.
func retryDelay(attempt int) time.Duration {
	if attempt > 8 {
		attempt = 8
	}
	d := retryInitial << uint(attempt)
	if d > retryMax {
		return retryMax
	}
	return d
}

// register POSTs the bound port until the env accepts it. 200 is the
// normal answer; 409 means a first register already won. 400 and 405 are
// contract errors and end the program; a network error or another status
// is retried, since the env may still be starting.
func register(envURL string, port int) error {
	client := &http.Client{Timeout: requestTimeout}
	payload, _ := json.Marshal(map[string]any{"port": port})
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequest("POST", envURL+"/register", bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		res, err := client.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, res.Body)
			_ = res.Body.Close()
			if res.StatusCode == 200 || res.StatusCode == 409 {
				return nil
			}
			if res.StatusCode == 400 || res.StatusCode == 405 {
				return fmt.Errorf("register answered HTTP %d", res.StatusCode)
			}
		}
		time.Sleep(retryDelay(attempt))
	}
}

// waitForDone polls GET /done until it answers {"done":true}; it never
// returns before that. A failed or slow poll is retried with a short
// exponential backoff.
func waitForDone(envURL string) {
	client := &http.Client{Timeout: requestTimeout}
	for attempt := 0; ; attempt++ {
		if pollDone(client, envURL) {
			return
		}
		time.Sleep(retryDelay(attempt))
	}
}

// pollDone is one GET /done attempt; it reports whether the env says done.
func pollDone(client *http.Client, envURL string) bool {
	res, err := client.Get(envURL + "/done")
	if err != nil {
		return false
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return false
	}
	var body struct {
		Done bool `json:"done"`
	}
	return json.NewDecoder(res.Body).Decode(&body) == nil && body.Done
}
