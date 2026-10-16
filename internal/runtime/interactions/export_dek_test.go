package interactions

// Bytes returns a copy of the key bytes, so a test can look for them in the
// database files.
func (k *DEK) Bytes() []byte { return append([]byte(nil), k.k[:]...) }

// SetBetweenQuerySteps sets the function query runs between reading its
// rows and opening them, and returns the function that restores it.
func SetBetweenQuerySteps(fn func()) (restore func()) {
	old := betweenQuerySteps
	betweenQuerySteps = fn
	return func() { betweenQuerySteps = old }
}
