package poller

import (
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

var EOF = io.EOF

type Context interface{}

type Worker interface {
	GetRescheduled() bool
	Lock()
	Unlock()
	RemoveConn(c *Conn)
	AddConn(c *Conn, toAwake bool) error
	IsAsync() bool
}

type Conn struct {
	*sync.Mutex
	Fd      int      /* 文件描述符 */
	Laddr   net.Addr /* 本地监听地址 */
	Raddr   net.Addr /* 远程客户端地址 */
	W       Worker   /* worker，绑定一个Poll对象的调度器 */
	Count   int64    /* 读取次数 */
	Score   int64
	Rlock   sync.Mutex
	Wlock   sync.Mutex
	Context Context
	Ready   int32
	Closing int32
	Closed  int32 /* 已关闭 */
}

// Read 从conn的fd中读取数据. 在buffio.Reader.fill()中调用
func (c *Conn) Read(b []byte) (n int, err error) {
	if len(b) == 0 {
		return 0, nil
	}
	c.Lock()
	if c.W.GetRescheduled() { /* 异步处理状态下，记录conn的读事件次数 */
		c.Unlock()
		atomic.AddInt64(&c.Count, 1)
	} else {
		c.Unlock()
	}
	c.Rlock.Lock()
	n, err = syscall.Read(c.Fd, b)
	c.Rlock.Unlock()
	if err != nil && err != syscall.EAGAIN || err == nil && n == 0 {
		err = EOF
	}
	if n < 0 {
		n = 0
	}
	return
}

// Write 向conn的fd写数据.
func (c *Conn) Write(b []byte) (n int, err error) {
	if len(b) == 0 {
		return 0, nil
	}
	var remain = len(b)
	c.Wlock.Lock()
	for remain > 0 {
		n, err = syscall.Write(c.Fd, b[len(b)-remain:])
		if n > 0 {
			remain -= n
			continue
		}
		if err != syscall.EAGAIN {
			c.Wlock.Unlock()
			return len(b) - remain, EOF
		}
	}
	c.Wlock.Unlock()
	return len(b), nil
}

// Close 关闭连接
func (c *Conn) Close() (err error) {
	if !atomic.CompareAndSwapInt32(&c.Closed, 0, 1) {
		return
	}
	return syscall.Close(c.Fd)
}

// LocalAddr 本地监听地址
func (c *Conn) LocalAddr() net.Addr {
	return c.Laddr
}

// RemoteAddr 远程客户端地址
func (c *Conn) RemoteAddr() net.Addr {
	return c.Raddr
}

func (c *Conn) SetDeadline(t time.Time) error {
	return errors.New("not supported")
}

func (c *Conn) SetReadDeadline(t time.Time) error {
	return errors.New("not supported")
}

func (c *Conn) SetWriteDeadline(t time.Time) error {
	return errors.New("not supported")
}
