//go:build !linux

package main

import "errors"

func processSample(pid int) (any, uint64, error) {
	return nil, 0, errors.New("process resource sampling requires Linux /proc")
}
