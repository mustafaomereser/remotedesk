//go:build darwin

package main

import "runtime"

// AppKit olay döngüsü programın ana iş parçacığında çalışmalı (desktop.RunMain).
func init() { runtime.LockOSThread() }
