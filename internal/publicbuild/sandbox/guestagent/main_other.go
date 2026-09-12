//go:build !linux

package main

import "fmt"

func main() {
	fmt.Println("the Public Build guest agent requires Linux")
}
