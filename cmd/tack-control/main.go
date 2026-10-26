//go:build go1.25

// tack-control combines hosted management and the Chisel server in one executable.
package main

import (
	"github.com/jpillora/chisel/internal/controlapp"
	"tack.local/management/control/command"
)

func main() { command.MainWithTunnel(controlapp.Support()) }
