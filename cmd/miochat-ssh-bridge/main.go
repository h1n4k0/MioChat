package main

import (
	"context"
	"fmt"
	"os"

	"github.com/mio-qwq/MioChat/internal/guitransport"
)

func main() {
	if err := guitransport.Run(context.Background(), os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
