package flowctl

import "errors"

var (
	ErrNegativeWindowUpdate  = errors.New("invalid window update, negative value")
	ErrFlowWindowIncOverflow = errors.New("invalid inc, flow control window overflow")
	ErrFlowControlUnderflow  = errors.New("invalid data, flow control window underflow")
)
