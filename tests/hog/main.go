// hog holds N MiB of incompressible memory and keeps touching every page, to create sustained memory pressure like the
// edge nodes see (a working set slightly larger than RAM). Test tool only; built into .vm/, never into the image.
package main

import (
	"crypto/rand"
	"os"
	"strconv"
	"time"
)

func main() {
	n, _ := strconv.Atoi(os.Args[1])
	b := make([]byte, n<<20)
	_, _ = rand.Read(b)
	for {
		for i := 0; i < len(b); i += 4096 {
			b[i]++
		}
		time.Sleep(5 * time.Millisecond)
	}
}
