// tack-node combines management and the Chisel client in one executable.
package main

import (
	"github.com/jpillora/chisel/internal/nodeapp"
	"tack.local/management/node/command"
)

func main() { command.MainWithRunner(nodeapp.Run) }
