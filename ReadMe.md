### NioGin

--------------

NioGin is a Non-blocking implementation of Gin.
All functionalities from Gin are supported by NioGin.
Only epoll is supported by Now, kqueue is on the way.

NioGin是一个异步http server，http协议处理、路由查找、中间件等都使用Gin框架的实现，只是底层的套接字管理替换为non-blocking io模式，即eventloop。所有的使用习惯与Gin均一致。

本项目主要参考和使用了Thanks To中的项目，适用于学习eventloop的实现，以及对单机性能要求较高的场景。代码注释和实现逻辑描述比较清楚，比nbio和gev学习起来更方便。性能理论上比gev要强，没有nbio混乱的engine，request读取使用的是官方包的方法，比较清晰；因此可以方便地利用Gin的router、middleware、Context封装，得到一个功能完善的http server。

--------------

### Install

go get github.com/moqsien/niogin

--------------

### Usage
Some simple examples as follows:
- nonTLS
```go
package main

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/moqsien/niogin/httpserver"
)

func main() {
	router := httpserver.New()
	router.SetPoll(true) // use epoll, otherwise use official http server
	router.GET("/test", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"test": "hello"})
	})
	router.POST("/p", func(c *gin.Context) {
		a, _ := c.GetPostForm("t")
		c.JSON(http.StatusOK, gin.H{"test": a})
	})
	err := router.Start("localhost:8080")
	if err != nil {
		log.Fatal(err)
		return
	}
}
```

- TLS
```go
package main

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/moqsien/niogin/httpserver"
)

func main() {
	certFile, keyFile string := "", ""
	router := httpserver.New()
	router.SetPoll(true)  // use epoll, otherwise use official http server which is similar to Gin.
	router.GET("/test", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"test": "hello"})
	})
	router.POST("/p", func(c *gin.Context) {
		a, _ := c.GetPostForm("t")
		c.JSON(http.StatusOK, gin.H{"test": a})
	})
	err := router.StartTLS("localhost:8080", certFile, keyFile)
	if err != nil {
		log.Fatal(err)
		return
	}
}
```
--------------

### ToDos
- [x] http
- [x] tls 
- [x] epoll
- [ ] kqueue
- [ ] websocket
- [ ] rpc
- [ ] jwt
- [ ] limiter

--------------

### Design

![eventloop](https://github.com/moqsien/niogin/blob/main/docs/design.png)

1、Listener放在独立的epoll_wait中，监听新的连接请求，accept获得新的连接后，分发到workers(工作循环)监听新连接的读写事件；

2、工作循环(workers)分为两类，即非共享型和共享型；

3、非共享型worker，在一个时刻只绑定和监听一个新连接；

4、共享型worker，在同一个时刻可以绑定和监听多个新连接，允许同时处理固定多个读写事件任务；

5、新连接分发逻辑是：优先分发到空闲的非共享型worker上，如果没有空闲的非共享型worker，则会优先分发到已绑定的连接数最少的共享型worker上；

6、每个连接在触发读事件时，该连接的请求数+1；会通过调度来把共享型worker上请求数排名靠前的连接交换到非共享型worker上；这样可以优先用性能更好的非共享型worker处理请求频繁的连接；

--------------

### Thanks To
[Meng Huang(hslam)](https://github.com/hslam/netpoll)
