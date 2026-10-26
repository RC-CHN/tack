// This development entrypoint serves management only. Official release builds
// use the root cmd/tack-control entrypoint with the embedded tunnel server.
package main

import "tack.local/management/control/command"

func main() { command.Main() }
