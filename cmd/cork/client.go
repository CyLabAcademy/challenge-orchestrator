package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// client is a minimal wrapper around the cmgrd HTTP API. Requests carry no
// timeout: update/build calls legitimately run for minutes (image builds and
// registry pushes happen synchronously on the server).
type client struct {
	base string
	http *http.Client
}

func newClient(base string) *client {
	return &client{base: strings.TrimRight(base, "/"), http: &http.Client{}}
}

// do sends a request and returns the response body. A non-2xx status is
// reported as an error containing the status and the server's message body.
func (c *client) do(method, path string, reqBody interface{}) ([]byte, error) {
	var bodyReader io.Reader
	if reqBody != nil {
		data, err := json.Marshal(reqBody)
		if err != nil {
			return nil, err
		}
		bodyReader = bytes.NewReader(data)
	}

	req, err := http.NewRequest(method, c.base+path, bodyReader)
	if err != nil {
		return nil, err
	}
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg := strings.TrimSpace(string(respBody))
		if msg == "" {
			msg = resp.Status
		} else {
			msg = fmt.Sprintf("%s: %s", resp.Status, msg)
		}
		return nil, fmt.Errorf("%s %s: %s", method, path, msg)
	}

	return respBody, nil
}

// doJSON sends a request and decodes the JSON response into out (skipped when
// out is nil).
func (c *client) doJSON(method, path string, reqBody, out interface{}) error {
	respBody, err := c.do(method, path, reqBody)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(respBody, out)
}

// prettyJSON re-indents a raw JSON body for terminal display.
func prettyJSON(data []byte) string {
	var buf bytes.Buffer
	if err := json.Indent(&buf, data, "", "  "); err != nil {
		return string(data)
	}
	return buf.String()
}
