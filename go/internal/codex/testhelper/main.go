package main

import (
	"fmt"
	"os"
	"os/exec"
)

func main() {
	if len(os.Args) < 2 {
		os.Exit(2)
	}
	switch os.Args[1] {
	case "leaf":
		select {}
	case "tree":
		if len(os.Args) != 3 {
			os.Exit(2)
		}
		child := exec.Command(os.Args[0], "leaf")
		if err := child.Start(); err != nil {
			os.Exit(3)
		}
		value := fmt.Sprintf("%d\n%d\n", os.Getpid(), child.Process.Pid)
		if err := os.WriteFile(os.Args[2], []byte(value), 0o600); err != nil {
			os.Exit(4)
		}
		select {}
	default:
		os.Exit(2)
	}
}
