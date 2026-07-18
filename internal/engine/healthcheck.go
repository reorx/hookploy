package engine

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/reorx/hookploy/internal/ops"
)

// healthcheck polls the URL until the expected status or retries exhausted.
func (e *Engine) healthcheck(ctx context.Context, idx int, a *ops.Healthcheck, sink Sink) error {
	var last string
	for attempt := 1; attempt <= a.Retries; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
		if err != nil {
			return err
		}
		resp, err := e.HTTP.Do(req)
		if err != nil {
			last = err.Error()
		} else {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			if resp.StatusCode == a.Expect {
				sink.Log(idx, "system", fmt.Sprintf("healthcheck OK: %s returned %d (attempt %d/%d)\n",
					a.URL, resp.StatusCode, attempt, a.Retries))
				return nil
			}
			last = fmt.Sprintf("status %d (want %d)", resp.StatusCode, a.Expect)
		}
		sink.Log(idx, "system", fmt.Sprintf("healthcheck attempt %d/%d: %s\n", attempt, a.Retries, last))
		if attempt < a.Retries {
			if err := e.sleep(ctx, time.Duration(a.Interval)); err != nil {
				return err
			}
		}
	}
	return fmt.Errorf("healthcheck failed after %d attempts: %s", a.Retries, last)
}
