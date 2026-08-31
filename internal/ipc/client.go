package ipc

import (
	"encoding/json"
	"net"
	"strings"
	"time"
)

const DefaultTimeout = 60 * time.Second

type Client struct {
	SocketPath string
	Timeout    time.Duration // 0 means DefaultTimeout
}

func (c *Client) effectiveTimeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	if c.Timeout < 0 {
		return 0 // no deadline
	}
	return DefaultTimeout
}

func (c *Client) Do(req Request) (Response, error) {
	socketPath := c.SocketPath
	if strings.HasPrefix(socketPath, "@") {
		socketPath = "\x00" + strings.TrimPrefix(socketPath, "@")
	}
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return Response{}, err
	}
	defer conn.Close()
	if d := c.effectiveTimeout(); d > 0 {
		_ = conn.SetDeadline(time.Now().Add(d))
	}

	encoder := json.NewEncoder(conn)
	decoder := json.NewDecoder(conn)

	if err := encoder.Encode(req); err != nil {
		return Response{}, err
	}

	var resp Response
	if err := decoder.Decode(&resp); err != nil {
		return Response{}, err
	}
	return resp, nil
}
