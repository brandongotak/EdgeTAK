//go:build !windows

package stream

import "syscall"

const errConnReset = syscall.ECONNRESET
