package sig

import (
	"fmt"
	"os"
	"sync"
	"syscall"

	"github.com/ethereum/go-ethereum/log"
)

var once sync.Once

func Int(reason string) {
	log.Info("### DEBUG ### Trying to shut down process by SIGINT", "reason", reason)
	once.Do(func() {
		err := syscall.Kill(os.Getpid(), syscall.SIGINT)
		if err != nil {
			fmt.Println("Error sending signal:", err)
		}
	})
}
