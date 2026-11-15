package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"sync"
	"syscall"
	"time"
)

type response struct {
	Target           string  `json:"target"`
	SiteName         string  `json:"site_name"`
	ThreadsConnected int     `json:"threads_connected"`
	Ms               float64 `json:"ms"`
	Error            string  `json:"error"`
}

type result struct {
	done    bool // false if the request never ran or was cut off by Ctrl+C
	latency time.Duration
	ok      bool
	errMsg  string
	threads int
}

func main() {
	concurrency := flag.Int("c", 1, "Number of concurrent workers")
	total := flag.Int("n", 0, "Total number of requests (default: same as -c)")
	baseURL := flag.String("url", "http://localhost:8086", "Target URL")
	mode := flag.String("mode", "proxy", "How PHP reaches MySQL: direct or proxy")
	timeout := flag.Duration("t", 10*time.Second, "Per-request timeout")
	flag.Parse()

	if *mode != "direct" && *mode != "proxy" {
		fmt.Fprintln(os.Stderr, "-mode must be direct or proxy")
		os.Exit(2)
	}
	u, err := url.Parse(*baseURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bad -url:", err)
		os.Exit(2)
	}
	q := u.Query()
	q.Set("mode", *mode)
	u.RawQuery = q.Encode()
	target := u.String()

	if *total <= 0 {
		*total = *concurrency
	}

	// ctx is cancelled on the first Ctrl+C (SIGINT) or SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		// Restore default behaviour: a second Ctrl+C kills the program immediately.
		stop()
		fmt.Fprintln(os.Stderr, "\nInterrupted: stopping... (Ctrl+C again to force quit)")
	}()

	fmt.Printf("Mode: %s | target: %s | concurrency: %d | requests: %d\n\n", *mode, target, *concurrency, *total)

	results, elapsed := run(ctx, target, *concurrency, *total, *timeout)
	report(results, *total, elapsed)
}

func run(ctx context.Context, target string, concurrency, total int, timeout time.Duration) ([]result, time.Duration) {
	client := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			MaxIdleConnsPerHost: concurrency,
		},
	}

	results := make([]result, total)
	jobs := make(chan int)
	var wg sync.WaitGroup

	start := time.Now()

	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				results[i] = doRequest(ctx, client, target)
			}
		}()
	}

	// Hand out jobs until done or interrupted.
feed:
	for i := 0; i < total; i++ {
		select {
		case jobs <- i:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()

	return results, time.Since(start)
}

func doRequest(ctx context.Context, client *http.Client, target string) result {
	start := time.Now()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return result{done: true, errMsg: "request: " + err.Error()}
	}

	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return result{} // cut off by Ctrl+C: not counted
		}
		return result{done: true, latency: time.Since(start), errMsg: "request: " + err.Error()}
	}
	defer resp.Body.Close()

	var body response
	decodeErr := json.NewDecoder(resp.Body).Decode(&body)
	io.Copy(io.Discard, resp.Body) // drain so the connection can be reused
	latency := time.Since(start)

	switch {
	case decodeErr != nil && errors.Is(decodeErr, context.Canceled):
		return result{}
	case decodeErr != nil:
		return result{done: true, latency: latency, errMsg: fmt.Sprintf("HTTP %d, bad JSON", resp.StatusCode)}
	case resp.StatusCode != http.StatusOK:
		return result{done: true, latency: latency, errMsg: fmt.Sprintf("HTTP %d: %s", resp.StatusCode, body.Error)}
	default:
		return result{done: true, latency: latency, ok: true, threads: body.ThreadsConnected}
	}
}

func report(results []result, total int, elapsed time.Duration) {
	var ok, failed, maxThreads int
	errorCounts := map[string]int{}
	latencies := make([]time.Duration, 0, len(results))

	for _, r := range results {
		if !r.done {
			continue
		}
		latencies = append(latencies, r.latency)
		if r.ok {
			ok++
			maxThreads = max(maxThreads, r.threads)
		} else {
			failed++
			errorCounts[r.errMsg]++
		}
	}

	completed := ok + failed
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })

	if completed < total {
		fmt.Printf("Completed:           %d of %d (interrupted)\n", completed, total)
	}
	fmt.Printf("Total time:          %v\n", elapsed.Round(time.Millisecond))
	fmt.Printf("Requests/sec:        %.1f\n", float64(completed)/elapsed.Seconds())
	fmt.Printf("Successful:          %d\n", ok)
	fmt.Printf("Failed:              %d\n", failed)
	fmt.Printf("Max MySQL threads:   %d\n", maxThreads)

	fmt.Println("\nLatency:")
	fmt.Printf("  min %v | p50 %v | p95 %v | p99 %v | max %v\n",
		percentile(latencies, 0), percentile(latencies, 50), percentile(latencies, 95),
		percentile(latencies, 99), percentile(latencies, 100))

	if len(errorCounts) > 0 {
		fmt.Println("\nErrors:")
		for msg, n := range errorCounts {
			fmt.Printf("  %5d  %s\n", n, msg)
		}
	}
}

// percentile expects a sorted slice.
func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(float64(len(sorted)-1) * p / 100)
	return sorted[idx].Round(time.Microsecond)
}
