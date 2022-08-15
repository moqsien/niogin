### NioGin

--------------

NioGin is a Non-blocking implementation of Gin.
All functionalities from Gin are supported by NioGin.
Only epoll is supported by Now, kqueue is on the way.

NioGin是一个异步http server，http协议处理、路由查找、中间件等都使用Gin框架的实现，只是底层的套接字管理替换为non-blocking io模式，即eventloop。所有的使用习惯与Gin均一致。

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

![eventloop]("https://github.com/moqsien/niogin/docs/design.png")
