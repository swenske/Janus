package api

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"
)

// GitHub's release downloads answered 504 now and then for hours (2026-11-08
// and 09, its status page green): a node updating from a release URL failed
// at the first one. A bundle file is asked again when the failure is one
// that passes - no answer, a timeout, 408, 429, 500, 502, 503 or 504 - after
// each of fetchWaits (a Retry-After up to maxRetryAfter instead), each new
// try said on the update's stream; a 404 or any other answer is final.
var fetchWaits = []time.Duration{5 * time.Second, 15 * time.Second, 30 * time.Second, 60 * time.Second}

const maxRetryAfter = time.Minute

type fetchNoticeKey struct{}

// withFetchNotice is ctx for a fetch whose new tries are said with notice -
// an update's progress stream.
func withFetchNotice(ctx context.Context, notice func(string)) context.Context {
	return context.WithValue(ctx, fetchNoticeKey{}, notice)
}

// passingStatus is an answer worth asking again for.
func passingStatus(code int) bool {
	switch code {
	case http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusInternalServerError,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// getRetrying GETs url, asking again after a failure that passes (fetchWaits).
// The response is a final one (a 200, a 404...), for the caller to read and
// close; the error, the last failure's when every try failed.
func getRetrying(ctx context.Context, url string) (*http.Response, error) {
	tries := len(fetchWaits) + 1
	for try := 1; ; try++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, fmt.Errorf("build request for %s: %w", url, err)
		}
		resp, err := http.DefaultClient.Do(req)
		var failure error
		wait := time.Duration(0)
		switch {
		case err != nil && ctx.Err() != nil:
			return nil, fmt.Errorf("GET %s: %w", url, err)
		case err != nil:
			failure = fmt.Errorf("GET %s: %w", url, err)
		case passingStatus(resp.StatusCode):
			failure = fmt.Errorf("GET %s: unexpected status %s", url, resp.Status)
			if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
				wait = min(time.Duration(s)*time.Second, maxRetryAfter)
			}
			resp.Body.Close()
		default:
			return resp, nil
		}
		if try == tries {
			return nil, fmt.Errorf("%w - %d tries", failure, tries)
		}
		if wait == 0 {
			wait = fetchWaits[try-1]
		}
		msg := fmt.Sprintf("%v - trying again in %s (%d/%d)", failure, wait, try+1, tries)
		log.Printf("lifecycle: %s", msg)
		if notice, ok := ctx.Value(fetchNoticeKey{}).(func(string)); ok {
			notice(msg)
		}
		t := time.NewTimer(wait)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return nil, fmt.Errorf("%w - stopped waiting: %w", failure, ctx.Err())
		}
	}
}
