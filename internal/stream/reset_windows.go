package stream

import "syscall"

// errConnReset is Windows' "connection forcibly closed by the remote host".
const errConnReset = syscall.WSAECONNRESET
