package poller

import (
	"errors"
	"sync"
	"syscall"
	"time"
)

type Mode int

const (
	READ             Mode = 1 << iota /* 读模式 */
	WRITE                             /* 写模式 */
	DefaultEventSize int  = 1024
	DefaultTimeout   int  = 1000
)

type Event struct {
	Fd   int  /* 文件描述符 */
	Mode Mode /* 读或写 */
}

var Tag = "epoll"

// 超时时间设置出错：超时时间必须大于1毫秒
var ErrTimeout = errors.New("non-positive interval for SetTimeout")

// Poll  epoll对象的封装
type Poll struct {
	efd     int        /* epoll对象的文件描述符 */
	pool    *sync.Pool /* epoll事件对象缓存池 */
	events  []syscall.EpollEvent
	timeout int
}

// New 创建Poll对象
func New() (*Poll, error) {
	efd, err := syscall.EpollCreate1(0)
	if err != nil {
		return nil, err
	}
	return &Poll{
		efd:    efd,
		events: make([]syscall.EpollEvent, DefaultEventSize),
		pool: &sync.Pool{New: func() interface{} {
			return syscall.EpollEvent{}
		}},
		timeout: DefaultTimeout,
	}, nil
}

// Register 注册文件描述符fd到epoll
func (p *Poll) Register(fd int) (err error) {
	event := p.pool.Get().(syscall.EpollEvent)
	event.Fd, event.Events = int32(fd), syscall.EPOLLIN
	err = syscall.EpollCtl(p.efd, syscall.EPOLL_CTL_ADD, fd, &event)
	p.pool.Put(event)
	return
}

// Unregister 将文件描述符fd从epoll监听列表移除
func (p *Poll) Unregister(fd int) (err error) {
	event := p.pool.Get().(syscall.EpollEvent)
	event.Fd, event.Events = int32(fd), syscall.EPOLLIN|syscall.EPOLLOUT
	err = syscall.EpollCtl(p.efd, syscall.EPOLL_CTL_DEL, fd, &event)
	p.pool.Put(event)
	return
}

// SetTimeout 设置超时时间，必须大于1毫秒
func (p *Poll) SetTimeout(d time.Duration) (err error) {
	if d < time.Millisecond {
		return ErrTimeout
	}
	p.timeout = int(d / time.Millisecond)
	return nil
}

// Close 关闭epoll文件描述符
func (p *Poll) Close() error {
	return syscall.Close(p.efd)
}

// Write 为文件描述符fd添加写事件.
func (p *Poll) Write(fd int) (err error) {
	event := p.pool.Get().(syscall.EpollEvent)
	event.Fd, event.Events = int32(fd), syscall.EPOLLIN|syscall.EPOLLOUT /* 同时监听fd的读和写事件 */
	err = syscall.EpollCtl(p.efd, syscall.EPOLL_CTL_MOD, fd, &event)
	p.pool.Put(event)
	return
}

// Wait 封装epoll_wait.
func (p *Poll) Wait(events []Event) (n int, err error) {
	if cap(p.events) >= len(events) {
		p.events = p.events[:len(events)]
	} else {
		p.events = make([]syscall.EpollEvent, len(events))
	}

	n, err = syscall.EpollWait(p.efd, p.events, p.timeout)
	if err != nil {
		if err != syscall.EINTR {
			return 0, err
		}
		err = nil
	}
	for i := 0; i < n; i++ {
		ev := p.events[i]

		events[i].Fd = int(ev.Fd)
		if ev.Events&syscall.EPOLLIN != 0 {
			events[i].Mode = READ /* 读事件 */
		} else if ev.Events&syscall.EPOLLOUT != 0 {
			events[i].Mode = WRITE /* 写事件 */

			// 注册ev.Fd读事件
			event := p.pool.Get().(syscall.EpollEvent)
			event.Fd, event.Events = ev.Fd, syscall.EPOLLIN
			syscall.EpollCtl(p.efd, syscall.EPOLL_CTL_MOD, int(ev.Fd), &event)
			p.pool.Put(event)
		}
	}
	return
}
