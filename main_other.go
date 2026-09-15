//go:build !linux

package main

import "fmt"

func main() {
	fmt.Println("pi-square: Linux is required")
}
