package ipc

import (
	"encoding/json"
	"net"
	"strings"
	"time"
)

type Client struct {
	SocketPath string
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
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

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
