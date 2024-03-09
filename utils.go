package mux

func add32(x, y int32) (int32, int32) {
	sum64 := int64(x) + int64(y)
	carryOut := int32(sum64>>32)
	return int32(sum64), carryOut
}
