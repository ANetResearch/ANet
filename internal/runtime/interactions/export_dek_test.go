package interactions

// Bytes returns a copy of the key bytes, so a test can look for them in the
// database files.
func (k *DEK) Bytes() []byte { return append([]byte(nil), k.k[:]...) }
