//go:build linux

package main

import "tack.local/testfixtures/resource"

func processSample(pid int) (any, uint64, error) {
	s, err := resource.Process(pid)
	return s, s.RSS, err
}
