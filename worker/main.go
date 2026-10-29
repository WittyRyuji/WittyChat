// WittyChat background worker entrypoint.
// WittyChat 后台任务进程入口。
package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	// Task loop lands in M6 (thumbnails, GC, mail parsing).
	// 任务循环在 M6 落地（缩略图、清理、邮件解析）。
	log.Println("worker started (task loop not implemented yet)")

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Println("worker stopped")
}
