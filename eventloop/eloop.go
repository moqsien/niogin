package eventloop

import (
	"errors"
	"net"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/moqsien/niogin/poller"
	"github.com/moqsien/niogin/toolkit/sort"
)

var numCPU = runtime.NumCPU()

// ErrHandler is the error when the Handler is nil
var ErrHandler = errors.New("Handler must be not nil")

// ErrListener is the error when the Listener is nil
var ErrListener = errors.New("Listener must be not nil")

// ErrServerClosed is returned by the Server's Serve and ListenAndServe
// methods after a call to Close.
var ErrServerClosed = errors.New("Server closed")

type List []*Conn

func (l List) Len() int { return len(l) }
func (l List) Less(i, j int) bool {
	return atomic.LoadInt64(&l[i].Score) < atomic.LoadInt64(&l[j].Score)
}
func (l List) Swap(i, j int) { l[i], l[j] = l[j], l[i] }

type otherLoop struct {
	listener net.Listener
	Handler  Handler
}

func (s *otherLoop) Serve(l net.Listener) (err error) {
	s.listener = l
	for {
		var conn net.Conn
		conn, err = s.listener.Accept()
		if err != nil {
			break
		}
		go func(c net.Conn) {
			var err error
			var context Context
			if context, err = s.Handler.Upgrade(c); err != nil {
				c.Close()
				return
			}
			for err == nil {
				err = s.Handler.Serve(context)
			}
			c.Close()
		}(conn)
	}
	return
}

func (s *otherLoop) Close() error {
	return s.listener.Close()
}

type EventLoop struct {
	*sync.Mutex                      /* 锁，当操作eventloop上的数据，如workers列表时，使用 */
	fd                int            /* listener的文件描述符fd */
	addr              net.Addr       /* 本地listener监听地址对象 */
	file              *os.File       /* listener对应的文件对象 */
	Network           string         /* 网络连接类型：tcp */
	Address           string         /* 本地监听地址 */
	Handler           Handler        /* 用于处理req的相关方法 */
	NoAsync           bool           /* 是否非异步，true则表示worker无法async，所有worker都只处理一个conn */
	UnsharableWorkers int            /* 非共享型worker数，用于包外部设置 */
	unsharableWorkers uint           /* 非共享型worker数 */
	SharableWorkers   int            /* 可共享型worker数，用于包外部设置 */
	sharableWorkers   uint           /* 可共享型worker数 */
	TasksPerWorker    int            /* 每个worker处理的任务数 */
	tasksPerWorker    uint           /* 每个worker处理的任务数 */
	otherLoop         *otherLoop     /* 其他服务类型 */
	poll              *poller.Poll   /* poll对象，对epoll的封装，用于监听listener的fd的事件 */
	workers           workers        /* 所有workers的队列，一个workers绑定一个epoll对象 */
	heap              workers        /* 共享型workers的队列 */
	list              List           /* 用于缓存所有workers中的conn */
	adjust            List           /* 用于缓存所有非共享型workers中的conn */
	wake              bool           /* 是否已唤醒重新调度，把请求多的conn放到非共享型worker上 */
	rescheduled       bool           /* 是否可重调度 */
	rescheduling      int32          /* 正在重新调度 */
	wg                sync.WaitGroup /* waitgroup 等待所有task goroutine执行完毕 */
	closed            int32          /* 是否已关闭 */
	done              chan struct{}  /* 结束信号 */
}

func (eloop *EventLoop) ListenAndServe() error {
	if atomic.LoadInt32(&eloop.closed) != 0 {
		return ErrServerClosed
	}
	ln, err := net.Listen(eloop.Network, eloop.Address)
	if err != nil {
		return err
	}
	return eloop.Serve(ln)
}

// 初始化一些默认配置
func (eloop *EventLoop) initEventLoop() {
	// 最大非共享型workers数目，默认16
	if eloop.UnsharableWorkers == 0 {
		eloop.unsharableWorkers = 16
	} else if eloop.UnsharableWorkers > 0 {
		eloop.unsharableWorkers = uint(eloop.UnsharableWorkers)
	}
	// 最大共享型workers数目，默认为cpu核数
	if eloop.SharableWorkers == 0 {
		eloop.sharableWorkers = uint(numCPU)
	} else if eloop.SharableWorkers > 0 {
		eloop.sharableWorkers = uint(eloop.SharableWorkers)
	} else {
		panic("SharedWorkers < 0")
	}
	// 每个woker允许的最大运行任务数目，默认为cpu核数；只有共享型worker才有task概念
	if eloop.TasksPerWorker == 0 {
		eloop.tasksPerWorker = uint(numCPU)
	} else if eloop.TasksPerWorker > 0 {
		eloop.tasksPerWorker = uint(eloop.TasksPerWorker)
	}
	eloop.Mutex = &sync.Mutex{}
}

func (eloop *EventLoop) Serve(l net.Listener) (err error) {
	if atomic.LoadInt32(&eloop.closed) != 0 {
		return ErrServerClosed
	}
	// 使用默认值初始化一些配置
	eloop.initEventLoop()
	if l == nil {
		return ErrListener
	} else if eloop.Handler == nil {
		return ErrHandler
	}
	switch netListener := l.(type) {
	case *net.TCPListener:
		if eloop.file, err = netListener.File(); err != nil {
			l.Close()
			return err
		}
	case *net.UnixListener:
		if eloop.file, err = netListener.File(); err != nil {
			l.Close()
			return err
		}
	default:
		eloop.otherLoop = &otherLoop{Handler: eloop.Handler}
		return eloop.otherLoop.Serve(l)
	}

	eloop.fd = int(eloop.file.Fd())
	eloop.addr = l.Addr() /* listener地址 */
	// 关闭listener，实际是将listener对应的fd解引用
	l.Close()
	if err := syscall.SetNonblock(eloop.fd, true); err != nil {
		return err
	}

	// 创建新的epoll，用于监听listener的fd
	if eloop.poll, err = poller.New(); err != nil {
		return err
	}
	// 注册listener的fd到epoll中，并监听读事件
	eloop.poll.Register(eloop.fd)
	// 默认为异步处理，eloop.NoAsync=false
	if !eloop.NoAsync && eloop.unsharableWorkers > 0 && eloop.sharableWorkers > 0 {
		// 是否可以重新调度
		eloop.rescheduled = true
	}

	// 创建workers，用于处理新连接(conn，或者说fd)的读写事件，默认创建24个
	totalWorkers := int(eloop.unsharableWorkers + eloop.sharableWorkers)
	for i := 0; i < totalWorkers; i++ {
		// 每个worker分配一个epoll
		p, err := poller.New()
		if err != nil {
			return err
		}

		var async bool
		if i >= int(eloop.unsharableWorkers) && !eloop.NoAsync {
			// 共享型workers设置为异步处理
			async = true
		}

		w := &worker{
			Mutex:  &sync.Mutex{},
			index:  i,                                         /* worker索引 */
			eloop:  eloop,                                     /* listener监听的事件循环 */
			conns:  make(map[int]*Conn),                       /* 连接列表 */
			poll:   p,                                         /* epoll对象封装 */
			events: make([]poller.Event, 0x400),               /* fd列表，也就是conn列表，初始长度1024 */
			async:  async,                                     /* 是否异步处理job */
			done:   make(chan struct{}, 1),                    /* task goroutine的结束信号 */
			jobs:   make(chan func()),                         /* job发送chan */
			tasks:  make(chan struct{}, eloop.tasksPerWorker), /* tasks数量限制，默认为cpu核数 */
		}

		// 填充全量workers队列，eventloop上所有的workers
		eloop.workers = append(eloop.workers, w) /* 前面都是非可共享workers */
		if i >= int(eloop.unsharableWorkers) {
			// 填充共享型workers队列eloop.heap，默认个数为cpu核数
			eloop.heap = append(eloop.heap, w)
		}
	}

	// eventloop结束的信号chan
	eloop.done = make(chan struct{}, 1)

	// 通过epoll来循环监听listener的读事件，进行accpet
	var n int
	var events = make([]poller.Event, 1) /* 每次获取一个listener的读事件 */
	for err == nil {
		if n, err = eloop.poll.Wait(events); n > 0 {
			if events[0].Fd == eloop.fd {
				err = eloop.accept()
			}
			// 唤醒调度器，按照conn的历史请求数(score)在非共享和共享worker间切换，
			// 历史请求多的放入非共享worker中
			eloop.wakeReschedule()
		}
		// 让出CPU时间
		runtime.Gosched()
	}

	// 如果listener监听出错，依然等待所有workers的tasks完成之后结束
	eloop.wg.Wait()
	return err
}

func (eloop *EventLoop) accept() (err error) {
	// 从listener的fd中accpet新连接
	nfd, sa, err := syscall.Accept(eloop.fd)
	if err != nil {
		if err == syscall.EAGAIN {
			return nil
		}
		return err
	}
	if err := syscall.SetNonblock(nfd, true); err != nil {
		return err
	}
	var raddr net.Addr /* 远程客户端地址 */
	switch sockaddr := sa.(type) {
	case *syscall.SockaddrUnix:
		raddr = &net.UnixAddr{Net: "unix", Name: sockaddr.Name}
	case *syscall.SockaddrInet4:
		raddr = &net.TCPAddr{
			IP:   append([]byte{}, sockaddr.Addr[:]...),
			Port: sockaddr.Port,
		}
	case *syscall.SockaddrInet6:
		var zone string
		if ifi, err := net.InterfaceByIndex(int(sockaddr.ZoneId)); err == nil {
			zone = ifi.Name
		}
		raddr = &net.TCPAddr{
			IP:   append([]byte{}, sockaddr.Addr[:]...),
			Port: sockaddr.Port,
			Zone: zone,
		}
	}

	eloop.Lock() /* 单线程，其实可以不用加锁 */
	// 选择一个worker，优先选择空闲的非共享型worker，其次是连接数最少的共享型worker
	w := eloop.assignWorker()
	// 将新连接注册到上面选择的worker的poll上，并唤醒worker
	err = w.Register(&Conn{W: w, Fd: nfd, Raddr: raddr, Laddr: eloop.addr, Mutex: &sync.Mutex{}})
	eloop.Unlock()

	return
}

func (eloop *EventLoop) assignWorker() (w *worker) {
	if w := eloop.idleUnsharedWorkers(); w != nil {
		// 优先选择空闲的非共享型worker
		return w
	}
	// 如果没有空闲的非共享型worker，则从共享型worker中选择连接数conn.count最小的
	return eloop.leastConnectedSharedWorkers()
}

func (eloop *EventLoop) idleUnsharedWorkers() (w *worker) {
	if eloop.unsharableWorkers > 0 {
		for i := 0; i < int(eloop.unsharableWorkers); i++ {
			//  非共享型，如果worker.count为0，说明该worker是空闲的
			if eloop.workers[i].count < 1 {
				return eloop.workers[i]
			}
		}
	}
	return nil
}

func (eloop *EventLoop) leastConnectedSharedWorkers() (w *worker) {
	// 最小堆排序
	sort.MinHeap(eloop.heap)
	// 取排序后最小值
	return eloop.heap[0]
}

func (eloop *EventLoop) wakeReschedule() {
	if !eloop.rescheduled {
		return
	}
	eloop.Lock()
	if !eloop.wake { /* 初始值为false */
		eloop.wake = true
		eloop.Unlock()
		go func() {
			ticker := time.NewTicker(time.Millisecond * 100)
			for {
				select {
				case <-ticker.C:
					eloop.Lock()
					// 重新调度
					stop := eloop.reschedule()
					if stop {
						eloop.wake = false
						ticker.Stop()
						eloop.Unlock()
						return
					}
					eloop.Unlock()
				case <-eloop.done:
					ticker.Stop()
					return
				}
				runtime.Gosched()
			}
		}()
	} else {
		eloop.Unlock()
	}
}

func (eloop *EventLoop) reschedule() (stop bool) {
	if !eloop.rescheduled {
		return
	}
	if !atomic.CompareAndSwapInt32(&eloop.rescheduling, 0, 1) {
		return false
	}
	defer atomic.StoreInt32(&eloop.rescheduling, 0)
	// 先清空eventloop上的缓存
	eloop.adjust = eloop.adjust[:0]
	eloop.list = eloop.list[:0]
	sum := int64(0)
	// 将所有workers中的conn读取到s.list中，
	// 其中所有非共享型workers中的conn同时存入s.adjust
	for idx, w := range eloop.workers { /* 非可共享workers在前端 */
		w.Lock()
		if !w.running {
			// 跳过已经关闭的workers
			w.Unlock()
			continue
		}
		for _, conn := range w.conns {
			if uint(idx) < eloop.unsharableWorkers {
				eloop.adjust = append(eloop.adjust, conn)
			}
			conn.Score = atomic.LoadInt64(&conn.Count)
			atomic.StoreInt64(&conn.Count, 0) /* 将conn.Count复制到conn.Score中，后面用conn.Score进行排序，保证排序不受conn.Count影响 */
			sum += conn.Score
			eloop.list = append(eloop.list, conn) /* 此时，非共享型worker的conn(也就是非async的conn)在列表前端 */
		}
		w.Unlock()
	}
	if len(eloop.list) == 0 || sum == 0 {
		// 如果总的conn个数为0或总请求次数为0，则停止调度
		return true
	}
	unsharedWorkers := eloop.unsharableWorkers
	if uint(len(eloop.list)) < eloop.unsharableWorkers {
		unsharedWorkers = uint(len(eloop.list))
	}

	// s.list按照conn的请求数的多少(score)进行排序，取请求数最多的前unsharableWorkers个conn
	sort.TopK(eloop.list, int(unsharedWorkers))
	index := 0

	// 遍历请求数最多的conn
	for _, conn := range eloop.list[:unsharedWorkers] {
		// 遍历到的可能都是非可共享workers的conn
		conn.Lock()
		if conn.W.IsAsync() {
			// 异步处理的都是可共享workers的conn，共享workers的索引大于unsharedWorkers
			conn.Unlock()
			// 异步处理的conn都放在s.list的前端
			eloop.list[index] = conn
			index++
		} else {
			conn.Unlock()
			if len(eloop.adjust) > 0 {
				for i := 0; i < len(eloop.adjust); i++ {
					if conn == eloop.adjust[i] {
						// 删除s.adjust中相同的元素
						if i < len(eloop.adjust)-1 {
							copy(eloop.adjust[i:], eloop.adjust[i+1:])
						}
						// 移除末尾的元素
						eloop.adjust = eloop.adjust[:len(eloop.adjust)-1]
						break
					}
				}
			}
		}
	}
	var reschedules = eloop.list[:index] /* 异步处理的请求最高的conn */
	if len(reschedules) == 0 || len(reschedules) != len(eloop.adjust) {
		return false
	}

	// 将共享型workers上的请求最多的conn与非共享型workers上的请求排名靠后的conn进行交换
	for i := 0; i < index; i++ {
		if atomic.LoadInt32(&eloop.adjust[i].Ready) == 0 || atomic.LoadInt32(&reschedules[i].Ready) == 0 {
			// 如果conn没有就绪，则跳过
			// conn未就绪，说明conn没有升级为Context或者conn.Fd没有设置为非阻塞
			continue
		}
		// ---------
		eloop.adjust[i].Lock()
		reschedules[i].Lock()

		unsharedWorker := eloop.adjust[i].W
		sharedWorker := reschedules[i].W

		// ========
		// unsharedWorker.lock.Lock()
		// sharedWorker.lock.Lock()
		unsharedWorker.Lock()
		sharedWorker.Lock()

		/* 此时的sharedWorker和unsharedWorker都是运行状态，不会出现死锁，
		因为worker在conn数量为0时，会自旋1s之后才进入睡眠状态，conn在worker之间的交换的完成时间远远小于1s */

		// 先将非共享型worker中的conn移动到共享型worker中
		unsharedWorker.RemoveConn(eloop.adjust[i])
		eloop.adjust[i].W = sharedWorker
		sharedWorker.AddConn(eloop.adjust[i], true)

		// 再将共享型中worker中请求数较多的conn移动到非共享型worker中
		sharedWorker.RemoveConn(reschedules[i])
		reschedules[i].W = unsharedWorker
		unsharedWorker.AddConn(reschedules[i], true)

		// sharedWorker.lock.Unlock()
		// unsharedWorker.lock.Unlock()
		sharedWorker.Unlock()
		unsharedWorker.Unlock()
		// =========

		eloop.adjust[i].Unlock()
		reschedules[i].Unlock()
		// ---------
	}
	return false
}
