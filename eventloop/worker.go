package eventloop

import (
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/moqsien/niogin/poller"
)

const (
	idleTime = time.Second
)

type worker struct {
	index int
	eloop *EventLoop /* listener监听事件循环 */
	count int64
	*sync.Mutex
	conns    map[int]*Conn
	lastIdle time.Time
	poll     *poller.Poll
	events   []poller.Event
	async    bool
	jobs     chan func()
	tasks    chan struct{}
	done     chan struct{}
	running  bool
	slept    int32
	closed   int32
}

type workers []*worker

func (l workers) Len() int { return len(l) }
func (l workers) Less(i, j int) bool {
	return l[i].count < l[j].count
}
func (l workers) Swap(i, j int) { l[i], l[j] = l[j], l[i] }

func (w *worker) IsAsync() bool {
	return w.async
}

func (w *worker) GetRescheduled() bool {
	return w.eloop.rescheduled
}

// serveConn conn交互逻辑
func (w *worker) serveConn(c *Conn) error {
	// 读写交互：fd与视图函数之间的交互
	for {
		err := w.eloop.Handler.Serve(c.Context) /* 正常完成时，err == EOF */
		if err != nil {
			if err == syscall.EAGAIN {
				return nil
			}
			if !atomic.CompareAndSwapInt32(&c.Closing, 0, 1) {
				return nil
			}
			w.RemoveConn(c)
			c.Close()
			return nil
		}
	}
}

// Register 将listener接收的conn注册到worker上
func (w *worker) Register(c *Conn) error {
	w.Lock()
	err := w.AddConn(c, false) /* 将conn的文件描述符注册到epoll中 */
	w.Unlock()
	// 第一个conn到来时，worker处于未运行状态，需要唤醒worker，从而开启epoll_wait事件循环
	w.wake() /* w.serve会加锁，可能会导致死锁 */

	/* worker已唤醒，则异步调用w.serveConn，先处理conn的本次请求，
	conn的后续请求则由worker的epoll_wait循环处理 */
	go func(w *worker, c *Conn) {
		var err error
		defer func() {
			if err != nil {
				w.AddConn(c, true)
				c.Close()
			}
		}()
		// if err = syscall.SetNonblock(c.Fd, false); err != nil {
		// 	return
		// }
		// 组装Context，与conn绑定
		if c.Context, err = w.eloop.Handler.Upgrade(c); err != nil {
			return
		}
		if err = syscall.SetNonblock(c.Fd, true); err != nil {
			return
		}
		// conn.Fd设置为非阻塞，Context组装好，表明conn已就绪
		atomic.StoreInt32(&c.Ready, 1)
		w.serveConn(c)
	}(w, c)
	return err
}

func (w *worker) AddConn(c *Conn, toAwake bool) error {
	w.conns[c.Fd] = c
	// worker上的连接数w.count +1
	atomic.AddInt64(&w.count, 1)
	// conn的fd注册到epoll
	w.poll.Register(c.Fd)
	if toAwake {
		w.wake()
	}
	return nil
}

func (w *worker) wake() {
	if !w.running {
		w.running = true
		w.done = make(chan struct{}, 1) /* task goroutine的关闭信号 */
		atomic.StoreInt32(&w.slept, 0)
		w.eloop.wg.Add(1)
		// 开启新goroutine，用于epoll_wait循环
		go w.run(&w.eloop.wg)
	}
}

func (w *worker) run(wg *sync.WaitGroup) {
	defer wg.Done()
	var n int
	var err error
	// epoll_wait事件循环
	for err == nil {
		n, err = w.poll.Wait(w.events)
		if n > 0 {
			for i := range w.events[:n] {
				ev := w.events[i]
				if w.async {
					// 共享型worker，异步处理，同时处理多个conn
					wg.Add(1)
					job := func() {
						w.serve(ev)
						wg.Done()
					}
					select {
					case w.jobs <- job:
						// 如果有task goroutine空闲，则job可以发送成功
					case w.tasks <- struct{}{}:
						// 最多新建len(w.tasks)个用于job执行的task goroutine
						go w.task(job)
					default:
						// 如果w.tasks已满，且都有job在执行，则新开一个goroutine执行job
						go job()
					}
				} else {
					// 非共享型worker，同步处理，只处理一个conn，
					// 在eloop.assignWorker中体现
					w.serve(ev)
				}
			}
		}

		// conn为0时，将worker休眠
		if atomic.LoadInt64(&w.count) < 1 {
			w.Lock()
			if len(w.conns) == 0 && w.lastIdle.Add(idleTime).Before(time.Now()) { /* 保持1s的自旋，1之后仍然没有conn注册进来，则worker进入休眠状态 */
				w.sleep() /* 关掉所有的task goroutine */
				w.running = false
				w.Unlock()
				return
			}
			w.Unlock()
		}
		runtime.Gosched()
	}
}

func (w *worker) task(job func()) {
	defer func() {
		// task被回收
		<-w.tasks
	}()

	for {
		job() /* w.serve(ev) */
		// 定时器，1s后到期
		t := time.NewTimer(idleTime)
		// 让出CPU时间
		runtime.Gosched()
		select {
		case job = <-w.jobs:
			// 当有新的job到来时，取消定时器
			t.Stop()
		case <-t.C:
			// 定时器1s到期，仍没有新的job到来，结束task goroutine
			return
		case <-w.done:
			// 当w.Close()调用时，结束task goroutine
			return
		}
	}
}

func (w *worker) serve(ev poller.Event) error {
	fd := ev.Fd
	if fd == 0 {
		return nil
	}
	w.Lock()
	c, ok := w.conns[fd]
	if !ok {
		w.Unlock()
		return nil
	}
	w.Unlock()
	if atomic.LoadInt32(&c.Ready) == 0 {
		// conn未就绪
		return nil
	}
	switch ev.Mode {
	case poller.WRITE:
	case poller.READ:
		w.serveConn(c)
	}
	return nil
}

func (w *worker) Unregister(c *Conn) {
	w.Lock()
	w.RemoveConn(c)
	w.Unlock()
}

func (w *worker) RemoveConn(c *Conn) {
	w.poll.Unregister(c.Fd)
	delete(w.conns, c.Fd)
	if atomic.AddInt64(&w.count, -1) < 1 {
		w.lastIdle = time.Now()
	}
}

func (w *worker) sleep() {
	if !atomic.CompareAndSwapInt32(&w.slept, 0, 1) { /* 设置worker为休眠状态 */
		return
	}
	close(w.done) /* 关闭所有task goroutine */
}

func (w *worker) Close() {
	if !atomic.CompareAndSwapInt32(&w.closed, 0, 1) {
		return
	}
	w.Lock()
	for _, c := range w.conns {
		c.Close()
		delete(w.conns, c.Fd)
	}
	w.sleep()
	w.poll.Close()
	w.Unlock()
}
