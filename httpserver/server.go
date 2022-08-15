package httpserver

import (
	"bufio"
	"crypto/tls"
	"net"
	"net/http"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/moqsien/niogin/eventloop"
	"github.com/moqsien/niogin/toolkit"
)

type Engine struct {
	*gin.Engine
	Handler   http.Handler
	TLSConfig *tls.Config
	fast      bool
	isPoll    bool
	mut       sync.Mutex
	listeners []net.Listener
	eloops    []*eventloop.EventLoop /* 事件循环列表 */
}

type Context struct {
	reader  *bufio.Reader
	rw      *bufio.ReadWriter
	conn    net.Conn
	serving sync.Mutex
}

func New() *Engine {
	return &Engine{
		Engine: gin.New(),
	}
}

var DefaultServer = New()

func ListenAndServe(addr string, handler http.Handler) error {
	s := DefaultServer
	s.Handler = handler
	return s.Start(addr)
}

func (server *Engine) Start(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return server.Serve(ln)
}

func (server *Engine) Serve(l net.Listener) error {
	return server.serve(l, server.TLSConfig)
}

func (server *Engine) serve(l net.Listener, config *tls.Config) error {
	// 事件循环server
	if server.isPoll {
		var handler = server.Handler
		if handler == nil {
			handler = server
		}
		var h = &eventloop.ConnHandler{}

		upgradeFunc := func(conn net.Conn) (eventloop.Context, error) {
			if config != nil {
				tlsConn := tls.Server(conn, config)
				if err := tlsConn.Handshake(); err != nil {
					conn.Close()
					return nil, err
				}
				conn = tlsConn
			}
			reader := bufio.NewReader(conn)
			rw := bufio.NewReadWriter(reader, bufio.NewWriter(conn))
			return &Context{reader: reader, conn: conn, rw: rw}, nil
		}

		h.SetUpgrade(upgradeFunc)

		serveFunc := func(context eventloop.Context) error {
			ctx := context.(*Context)
			var err error
			var req *http.Request

			// TODO: 锁的粒度还可以拆分？读和写分开
			ctx.serving.Lock()
			// 读取和解析tcp请求
			if server.fast {
				req, err = ReadFastRequest(ctx.reader)
			} else {
				req, err = http.ReadRequest(ctx.reader)
			}
			if err != nil {
				ctx.serving.Unlock()
				return err
			}
			res := NewResponse(req, ctx.conn, ctx.rw)
			// 进入用户编写的视图函数进行业务处理
			handler.ServeHTTP(res, req)
			res.FinishRequest()
			ctx.serving.Unlock()

			if server.fast {
				FreeRequest(req)
			}
			FreeResponse(res)
			return nil
		}
		h.SetServe(serveFunc)

		// listener监听用的事件循环
		eloop := &eventloop.EventLoop{
			Handler: h,
		}
		// listener监听用的事件循环加入事件循环列表
		server.mut.Lock()
		server.eloops = append(server.eloops, eloop)
		server.mut.Unlock()
		// 将监听套接字委托给事件循环
		return eloop.Serve(l)
	}

	// 普通server，一个conn用一个goroutine处理
	if config != nil {
		l = tls.NewListener(l, config)
	}
	// 加入listeners列表
	server.mut.Lock()
	server.listeners = append(server.listeners, l)
	server.mut.Unlock()
	for {
		conn, err := l.Accept()
		if err != nil {
			return err
		}
		go server.serveConn(conn)
	}
}

func (m *Engine) serveConn(conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	rw := bufio.NewReadWriter(reader, bufio.NewWriter(conn))
	var err error
	var req *http.Request
	var handler = m.Handler
	if handler == nil {
		handler = m
	}
	for {
		if m.fast {
			req, err = ReadFastRequest(reader)
		} else {
			req, err = http.ReadRequest(reader)
		}
		if err != nil {
			break
		}
		res := NewResponse(req, conn, rw)
		handler.ServeHTTP(res, req)
		if m.fast {
			FreeRequest(req)
		}
		res.FinishRequest()
		FreeResponse(res)
	}
}

// SetFast 设置fast参数，fast=true则使用自定义的request解析
func (server *Engine) SetFast(fast bool) {
	server.fast = fast
}

// SetPoll 设置isPoll参数，isPoll=true则使用epoll事件SetPoll
func (server *Engine) SetPoll(isPoll bool) {
	server.isPoll = isPoll
}

func ListenAndServeTLS(addr, certFile, keyFile string, handler http.Handler) error {
	s := DefaultServer
	s.Handler = handler
	return s.StartTLS(addr, certFile, keyFile)
}

// RunTLS is like Run but with a cert file and a key file.
func (server *Engine) StartTLS(addr string, certFile, keyFile string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return server.ServeTLS(ln, certFile, keyFile)
}

func (server *Engine) ServeTLS(l net.Listener, certFile, keyFile string) error {
	config := server.TLSConfig
	if config == nil {
		config = &tls.Config{}
	}
	if !toolkit.StrSliceContains(config.NextProtos, "http/1.1") {
		config.NextProtos = append(config.NextProtos, "http/1.1")
	}
	configHasCert := len(config.Certificates) > 0 || config.GetCertificate != nil
	if !configHasCert || certFile != "" || keyFile != "" {
		var err error
		config.Certificates = make([]tls.Certificate, 1)
		config.Certificates[0], err = tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return err
		}
	}
	return server.serve(l, config)
}
