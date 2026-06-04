package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type ResponsePoster interface {
	Post(context.Context, string, []byte) error
}

type HTTPResponsePoster struct {
	Client *http.Client
}

func (p HTTPResponsePoster) Post(ctx context.Context, responseURL string, body []byte) error {
	if responseURL == "" {
		return fmt.Errorf("response_url is required")
	}
	if !json.Valid(body) {
		return fmt.Errorf("Slack response body must be valid JSON")
	}

	client := p.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, responseURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create Slack response request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("post Slack response: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("post Slack response returned status %d", resp.StatusCode)
	}
	return nil
}
