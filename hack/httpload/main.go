// Command httpload sends GET requests over keep-alive connections for a
// while and prints how many it got answered - the QEMU tests' load
// generator, so they need no wrk on the runners.
//
// Usage: go run ./hack/httpload -url http://127.0.0.1:8080/ -c 64 -d 20s
//
// Prints one line: "requests N errors E rps R".
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

func main() {
	url := flag.String("url", "", "URL to GET")
	conns := flag.Int("c", 64, "connections")
	dur := flag.Duration("d", 20*time.Second, "how long")
	flag.Parse()
	if *url == "" {
		fmt.Fprintln(os.Stderr, "usage: httpload -url URL [-c N] [-d DURATION]")
		os.Exit(2)
	}

	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        *conns,
			MaxIdleConnsPerHost: *conns,
			MaxConnsPerHost:     *conns,
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), *dur)
	defer cancel()
	var ok, failed atomic.Int64
	var wg sync.WaitGroup
	start := time.Now()
	for range *conns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				req, _ := http.NewRequestWithContext(ctx, http.MethodGet, *url, nil)
				resp, err := client.Do(req)
				if err != nil {
					if ctx.Err() == nil {
						failed.Add(1)
					}
					continue
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					ok.Add(1)
				} else {
					failed.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	secs := time.Since(start).Seconds()
	fmt.Printf("requests %d errors %d rps %.0f\n", ok.Load(), failed.Load(), float64(ok.Load())/secs)
}
