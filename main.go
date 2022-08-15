package main

import (
	"log"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/moqsien/niogin/httpserver"
)

func main() {
	router := httpserver.New()
	router.SetPoll(true)
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
