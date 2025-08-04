package utils

import (
	"os"
	"os/signal"
	"syscall"
)

func CloseSignal() {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	<-sigs
}
