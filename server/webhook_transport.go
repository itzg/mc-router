package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/sirupsen/logrus"
)

// webhookClient is the shared HTTP transport for the two webhook subsystems,
// owning the http.Client, static headers, and the marshal→POST plumbing. The
// notifier fires and forgets via postAsync; the scaler drives control flow via
// postSync and reads the typed response body itself.
type webhookClient struct {
	client  *http.Client
	headers map[string]string
}

func newWebhookClient(timeout time.Duration, headers map[string]string) *webhookClient {
	return &webhookClient{
		client:  &http.Client{Timeout: timeout},
		headers: headers,
	}
}

func (w *webhookClient) newRequest(ctx context.Context, url string, payload any) (*http.Request, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal webhook payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create webhook request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range w.headers {
		req.Header.Set(k, v)
	}
	return req, nil
}

// postAsync sends in the background; a transport error or >=400 is logged, not returned.
func (w *webhookClient) postAsync(ctx context.Context, url string, payload any) error {
	req, err := w.newRequest(ctx, url, payload)
	if err != nil {
		return err
	}
	go func() {
		resp, err := w.client.Do(req)
		if err != nil {
			logrus.WithError(err).Warn("Failed to send webhook notification")
			return
		}
		_ = resp.Body.Close()
		if resp.StatusCode >= 400 {
			logrus.WithField("status", resp.StatusCode).Warn("webhook receiver responded with an error")
		}
	}()
	return nil
}

// postSync blocks for the response, mapping >=400 to an error. On nil error the caller closes resp.Body.
func (w *webhookClient) postSync(ctx context.Context, url string, payload any) (*http.Response, error) {
	req, err := w.newRequest(ctx, url, payload)
	if err != nil {
		return nil, err
	}
	resp, err := w.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		//goland:noinspection GoUnhandledErrorResult
		_ = resp.Body.Close()
		return nil, fmt.Errorf("webhook responded with status %d", resp.StatusCode)
	}
	return resp, nil
}
