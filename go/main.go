package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// hero is the trial lifecycle in Go: bind an ephemeral port, register it with
// the suite, then long-poll until the suite reports it is done.
func main() {
	envURL := os.Getenv("MAKEFUN_ENV_URL")
	if envURL == "" {
		fmt.Fprintln(os.Stderr, "forgepulse: MAKEFUN_ENV_URL is required")
		os.Exit(1)
	}
	token := os.Getenv("MAKEFUN_SHOP_TOKEN")
	viewer := os.Getenv("FORGE_OWNER")
	if viewer == "" {
		viewer = "adaiversen"
	}
	now := time.Now().UTC()
	if stamp := os.Getenv("MAKEFUN_TRIAL_NOW"); stamp != "" {
		if parsed, err := time.Parse(time.RFC3339, stamp); err == nil {
			now = parsed.UTC()
		}
	}

	store := NewStore(User{Login: viewer, ID: 1, NodeID: nodeID("User", 1), Type: "User"}, now)
	server := NewServer(store, token)

	mux := http.NewServeMux()
	mux.HandleFunc("/", server.Handle)
	// Keep-alive settings that suit a hot loop of small requests.
	httpServer := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      120 * time.Second,
	}

	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		fmt.Fprintln(os.Stderr, "forgepulse: listen:", err)
		os.Exit(1)
	}
	port := listener.Addr().(*net.TCPAddr).Port

	go func() {
		if err := httpServer.Serve(listener); err != nil && err != http.ErrServerClosed {
			fmt.Fprintln(os.Stderr, "forgepulse: serve:", err)
		}
	}()

	if err := registerWithTrial(envURL, port); err != nil {
		fmt.Fprintln(os.Stderr, "forgepulse:", err)
		os.Exit(1)
	}
	pollUntilDone(envURL)
	_ = httpServer.Close()
}

// registerWithTrial reports the listening port, retrying while the suite boots.
func registerWithTrial(envURL string, port int) error {
	endpoint := strings.TrimRight(envURL, "/") + "/register"
	payload, _ := json.Marshal(map[string]int{"port": port})
	client := &http.Client{Timeout: 2 * time.Second}
	delay := 25 * time.Millisecond
	for attempt := 0; attempt < 12; attempt++ {
		resp, err := client.Post(endpoint, "application/json", bytes.NewReader(payload))
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			switch {
			case resp.StatusCode == http.StatusOK, resp.StatusCode == http.StatusConflict:
				return nil
			case resp.StatusCode >= 400 && resp.StatusCode < 500:
				return fmt.Errorf("trial registration refused with %d", resp.StatusCode)
			}
		}
		time.Sleep(delay)
		if delay < 500*time.Millisecond {
			delay *= 2
			if delay > 500*time.Millisecond {
				delay = 500 * time.Millisecond
			}
		}
	}
	return fmt.Errorf("could not register with the trial environment")
}

// pollUntilDone waits for the suite to report completion, then returns so main
// can exit 0.
func pollUntilDone(envURL string) {
	endpoint := strings.TrimRight(envURL, "/") + "/done"
	client := &http.Client{Timeout: 2 * time.Second}
	interval := 100 * time.Millisecond
	for {
		time.Sleep(interval)
		resp, err := client.Get(endpoint)
		if err == nil {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				var out struct {
					Done bool `json:"done"`
				}
				if json.Unmarshal(body, &out) == nil && out.Done {
					return
				}
			}
		}
		if interval < time.Second {
			interval *= 2
			if interval > time.Second {
				interval = time.Second
			}
		}
	}
}
