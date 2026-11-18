package blob

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
)

// downloadBytes fetches a blob into memory. It is context-aware so a
// cancelled resolve does not leave work running.
func downloadBytes(ctx context.Context, blobURL string, headers map[string]string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, blobURL, nil)
	if err != nil {
		return nil, err
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("failed to get blob %s: %d %s: %s", blobURL, resp.StatusCode, http.StatusText(resp.StatusCode), string(body))
	}

	return io.ReadAll(resp.Body)
}

func detectContentType(data []byte) string {
	buf := data
	if len(buf) > 512 {
		buf = buf[:512]
	}
	return http.DetectContentType(buf)
}

func isTarBytes(data []byte) bool {
	_, err := tar.NewReader(bytes.NewReader(data)).Next()
	return err == nil
}
