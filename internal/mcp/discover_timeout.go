package mcp

import (
	"context"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// discoverTimeout 是 stdio 上 server/discover 探测的等待上限
const discoverTimeout = 10 * time.Second

// discoverTimeoutTransport 给 stdio 连接的 server/discover 探测加超时。
//
// 有些老服务器对 initialize 之前收到的未知方法既不报错也不回应，SDK 的探测会一直等下去。
// 探测超时后这里替服务器补一个 JSON-RPC 错误响应，SDK 收到错误就按老服务器处理，
// 在同一个子进程上回退 initialize 握手。
type discoverTimeoutTransport struct {
	mcp.Transport
}

func (t *discoverTimeoutTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	conn, err := t.Transport.Connect(ctx)
	if err != nil {
		return nil, err
	}
	c := &discoverTimeoutConn{Connection: conn, incoming: make(chan readResult), done: make(chan struct{})}
	go c.pump()
	return c, nil
}

type readResult struct {
	msg jsonrpc.Message
	err error
}

type discoverTimeoutConn struct {
	mcp.Connection
	// incoming 汇合底层读到的消息和超时补发的错误响应
	incoming  chan readResult
	done      chan struct{}
	closeOnce sync.Once

	mu sync.Mutex
	// probing：探测已发出、还在等响应；expired：探测已按超时处理，它迟到的响应要丢掉
	probeID jsonrpc.ID
	probing bool
	expired bool
}

// pump 把底层连接的阻塞读转成 channel，好和超时补发的响应一起 select
func (c *discoverTimeoutConn) pump() {
	for {
		msg, err := c.Connection.Read(context.Background())
		if resp, ok := msg.(*jsonrpc.Response); ok && c.isLateProbeResponse(resp.ID) {
			continue
		}
		select {
		case c.incoming <- readResult{msg, err}:
		case <-c.done:
			return
		}
		if err != nil {
			return
		}
	}
}

func (c *discoverTimeoutConn) isLateProbeResponse(id jsonrpc.ID) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if id != c.probeID {
		return false
	}
	late := c.expired
	c.probing, c.expired = false, false
	return late
}

func (c *discoverTimeoutConn) Read(ctx context.Context) (jsonrpc.Message, error) {
	select {
	case r := <-c.incoming:
		return r.msg, r.err
	case <-c.done:
		return nil, mcp.ErrConnectionClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *discoverTimeoutConn) Write(ctx context.Context, msg jsonrpc.Message) error {
	if req, ok := msg.(*jsonrpc.Request); ok && req.Method == "server/discover" && req.ID.IsValid() {
		c.mu.Lock()
		c.probeID, c.probing, c.expired = req.ID, true, false
		c.mu.Unlock()
		id := req.ID
		time.AfterFunc(discoverTimeout, func() { c.expire(id) })
	}
	return c.Connection.Write(ctx, msg)
}

// expire 在探测超时仍未收到响应时，补一个错误响应交给 SDK
func (c *discoverTimeoutConn) expire(id jsonrpc.ID) {
	c.mu.Lock()
	timedOut := c.probing && c.probeID == id
	if timedOut {
		c.probing, c.expired = false, true
	}
	c.mu.Unlock()
	if !timedOut {
		return
	}
	resp := &jsonrpc.Response{ID: id, Error: &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: "server/discover timed out"}}
	select {
	case c.incoming <- readResult{msg: resp}:
	case <-c.done:
	}
}

func (c *discoverTimeoutConn) Close() error {
	c.closeOnce.Do(func() { close(c.done) })
	return c.Connection.Close()
}
